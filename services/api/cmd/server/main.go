package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/billing"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/documents"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/entities"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/findings"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/mcp"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/review"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/tenant"
)

func main() {
	ctx := context.Background()

	// Initialize OpenTelemetry
	tp, err := initTracer(ctx)
	if err != nil {
		slog.Error("failed to initialize tracer", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			slog.Error("tracer shutdown error", "error", err)
		}
	}()

	// Initialize services
	authSvc := auth.NewService()
	tenantSvc := tenant.NewService()
	docSvc := documents.NewService()
	entitySvc := entities.NewService()
	findingSvc := findings.NewService()
	reviewSvc := review.NewService()
	billingSvc := billing.NewService()
	mcpSvc := mcp.NewService()

	// Router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"}, // configure via env in production
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Idempotency-Key"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(middleware.TraceInjector)
	r.Use(middleware.RLSInjector)

	// Health
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Check DB, NATS, gRPC connections
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})

	// Auth routes (public)
	r.Route("/v1/auth", func(r chi.Router) {
		r.Post("/signup", authSvc.HandleSignup)
		r.Post("/login", authSvc.HandleLogin)
		r.Post("/logout", authSvc.HandleLogout)
		r.Post("/refresh", authSvc.HandleRefresh)
		r.Get("/verify-email", authSvc.HandleVerifyEmail)
		r.Post("/forgot-password", authSvc.HandleForgotPassword)
		r.Post("/reset-password", authSvc.HandleResetPassword)
		r.Post("/totp/enable", authSvc.HandleEnableTOTP)
		r.Post("/totp/verify", authSvc.HandleVerifyTOTP)
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticator(authSvc))

		// Tenant/Book management
		r.Route("/v1/books", func(r chi.Router) {
			r.Get("/", tenantSvc.HandleListBooks)
			r.Post("/", tenantSvc.HandleCreateBook)
			r.Get("/{bookId}", tenantSvc.HandleGetBook)
			r.Patch("/{bookId}/settings", tenantSvc.HandleUpdateBookSettings)
			r.Post("/{bookId}/staff", tenantSvc.HandleAssignStaff)
			r.Delete("/{bookId}/staff/{userId}", tenantSvc.HandleRemoveStaff)
		})

		// Documents
		r.Route("/v1/books/{bookId}/documents", func(r chi.Router) {
			r.Post("/", docSvc.HandleUpload)
			r.Get("/", docSvc.HandleList)
			r.Get("/{docId}", docSvc.HandleGet)
			r.Get("/{docId}/view", docSvc.HandlePresignedView)
		})

		// Entities
		r.Get("/v1/books/{bookId}/entities", entitySvc.HandleList)

		// Review queue
		r.Get("/v1/books/{bookId}/review-queue", reviewSvc.HandleList)
		r.Post("/v1/entity-links/{linkId}/confirm", reviewSvc.HandleConfirm)
		r.Post("/v1/entity-links/{linkId}/reject", reviewSvc.HandleReject)
		r.Post("/v1/books/{bookId}/review-queue/bulk-confirm", reviewSvc.HandleBulkConfirm)

		// Findings
		r.Get("/v1/books/{bookId}/findings", findingSvc.HandleList)
		r.Post("/v1/findings/{findingId}/comments", findingSvc.HandleAddComment)
		r.Patch("/v1/findings/{findingId}/status", findingSvc.HandleUpdateStatus)
		r.Post("/v1/findings/{findingId}/attachments", findingSvc.HandleAddAttachment)

		// Reports
		r.Post("/v1/books/{bookId}/reports", findingSvc.HandleGenerateReport)
		r.Get("/v1/reports/{reportId}", findingSvc.HandleGetReport)
		r.Get("/v1/reports/{reportId}/citation/{findingId}", findingSvc.HandleGetCitation)

		// Billing
		r.Post("/v1/billing/checkout", billingSvc.HandleCheckout)
		r.Post("/v1/webhooks/stripe", billingSvc.HandleStripeWebhook)

		// MCP tools (internal, called by agent-runtime)
		r.Route("/mcp", func(r chi.Router) {
			r.Post("/tools/get_pending_entities", mcpSvc.HandleGetPendingEntities)
			r.Post("/tools/create_entity_link", mcpSvc.HandleCreateEntityLink)
			r.Post("/tools/flag_for_review", mcpSvc.HandleFlagForReview)
			r.Post("/tools/get_book_tolerance", mcpSvc.HandleGetBookTolerance)
		})

		// Firm admin
		r.Route("/v1/admin", func(r chi.Router) {
			r.Use(middleware.RequireRole("firm_admin"))
			r.Get("/team", tenantSvc.HandleListStaff)
			r.Get("/settings", tenantSvc.HandleGetFirmSettings)
			r.Patch("/settings", tenantSvc.HandleUpdateFirmSettings)
		})
	})

	// Server
	srv := &http.Server{
		Addr:         ":8080",
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down server")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
	}()

	slog.Info("server starting", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func initTracer(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "jaeger:4317"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("ai-auditor-api"),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp, nil
}