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
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/billing"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/documents"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/email"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/pipeline"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/entities"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/findings"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/humanoverride"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/mcp"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/notify"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/periods"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/portal"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/push"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/review"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/settings"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/storage"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/webhooks"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/tenant"
)

func main() {
	ctx := context.Background()

	// Initialize OpenTelemetry tracing (non-fatal if Jaeger unavailable)
	if tp, err := initTracer(ctx); err == nil {
		defer func() {
			if err := tp.Shutdown(ctx); err != nil {
				slog.Warn("tracer shutdown", "error", err)
			}
		}()
	} else {
		slog.Warn("tracing disabled", "error", err)
	}

	// Initialize database pool
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://auditor:auditor@localhost:5432/ai_auditor?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Seed chart-of-accounts templates (idempotent; warn-only on failure)
	if err := settings.SeedTemplates(ctx, pool); err != nil {
		slog.Warn("failed to seed COA templates", "error", err)
	}

	// Proactive stale document-request reminder loop (doc 10 §7)
	go notify.Run(ctx, pool, notify.DefaultInterval)

	// Initialize pipeline event client (NATS JetStream)
	var pipelineClient *pipeline.EventClient
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		pc, err := pipeline.NewEventClient(natsURL)
		if err != nil {
			slog.Warn("NATS unavailable — pipeline events disabled", "error", err)
		} else {
			pipelineClient = pc
			defer pc.Close()
		}
	}

	// Initialize services
	authSvc := auth.NewService()
	authSvc.SetDB(pool)
	authSvc.SetEmailSender(email.NewResend())

	tenantSvc := tenant.NewService()
	tenantSvc.SetDB(pool)

	docSvc := documents.NewService()
	docSvc.SetDB(pool)
	docSvc.SetPipeline(pipelineClient)
	var st *storage.Client
	if sc, err := storage.New(); err == nil {
		_ = sc.EnsureBucketExists(ctx)
		docSvc.SetStorage(sc)
		st = sc
	} else {
		slog.Warn("storage (MinIO/S3) unavailable — uploads limited", "error", err)
	}

	// Pipeline coordinator: consumes document.uploaded -> ingestion gRPC ->
	// extracted_entities -> entity.extraction.requested (doc 12 §1).
	if pipelineClient != nil && st != nil {
		if ingURL := os.Getenv("INGESTION_GRPC_ADDR"); ingURL != "" {
			coord, err := pipeline.NewCoordinator(os.Getenv("NATS_URL"), ingURL, pool, st)
			if err != nil {
				slog.Warn("pipeline coordinator unavailable", "error", err)
			} else {
				go func() {
					if err := coord.Run(ctx); err != nil {
						slog.Error("pipeline coordinator stopped", "error", err)
					}
				}()
			}
		} else {
			slog.Warn("INGESTION_GRPC_ADDR unset — coordinator not started")
		}
	}

	entitySvc := entities.NewService()
	entitySvc.SetDB(pool)
	findingSvc := findings.NewService()
	findingSvc.SetDB(pool)
	reviewSvc := review.NewService()
	reviewSvc.SetDB(pool)
	billingSvc := billing.NewService()

	periodsSvc := periods.NewService()
	periodsSvc.SetDB(pool)

	settingsSvc := settings.NewService()
	settingsSvc.SetDB(pool)
	billingSvc.SetDB(pool)
	mcpSvc := mcp.NewService()
	mcpSvc.SetDB(pool)

	webhooksSvc := webhooks.NewService()
	webhooksSvc.SetDB(pool)
	findingSvc.Notifier = webhooksSvc

	portalSvc := portal.NewService()
	portalSvc.SetDB(pool)
	portalSvc.SetAuth(authSvc)

	pushSvc := push.NewService()
	pushSvc.SetDB(pool)

	humanSvc := humanoverride.NewService()
	humanSvc.SetDB(pool)

	// Router
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"}, // configure via env in production
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Idempotency-Key"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(middleware.TraceInjector)

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

	// Client portal login (public — invite-token based, doc 07 §5)
	r.Post("/v1/portal/login", portalSvc.HandleLogin)

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
		// RLSInjector must run AFTER Authenticator sets firm/user/role in context.
		r.Use(middleware.RLSInjector(pool))

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
			r.With(middleware.Idempotency(pool)).Post("/", docSvc.HandleUpload)
			r.Post("/upload-url", docSvc.HandlePresignUpload)
			r.Get("/", docSvc.HandleList)
			r.Get("/{docId}", docSvc.HandleGet)
			r.Get("/{docId}/view", docSvc.HandlePresignedView)
			r.Post("/{docId}/confirm-upload", docSvc.HandleConfirmUpload)
		})

		// Entities
		r.Get("/v1/books/{bookId}/entities", entitySvc.HandleList)

		// Human override (doc 11) — manual entity creation + group split/merge
		r.Post("/v1/books/{bookId}/entities/manual", humanSvc.HandleCreateManualEntity)
		r.Post("/v1/reconciliation-groups/{groupId}/split", humanSvc.HandleSplitGroup)
		r.Post("/v1/reconciliation-groups/merge", humanSvc.HandleMergeGroups)

		// Config change history (doc 11 §3)
		r.Get("/v1/books/{bookId}/config-history", humanSvc.HandleConfigHistory)

		// Automation rate (doc 11 §5)
		r.Get("/v1/books/{bookId}/automation-rate", humanSvc.HandleAutomationRate)

		// Tags (doc 11 §6)
		r.Get("/v1/tags", humanSvc.HandleListTags)
		r.Post("/v1/tags", humanSvc.HandleCreateTag)
		r.Post("/v1/entities/tag", humanSvc.HandleTagEntity)

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
		r.With(middleware.Idempotency(pool)).Post("/v1/books/{bookId}/reports", findingSvc.HandleGenerateReport)
		r.Get("/v1/reports/{reportId}", findingSvc.HandleGetReport)
		r.Get("/v1/reports/{reportId}/citation/{findingId}", findingSvc.HandleGetCitation)

		// Billing
		r.Post("/v1/billing/checkout", billingSvc.HandleCheckout)
		r.Post("/v1/webhooks/stripe", billingSvc.HandleStripeWebhook)

		// Periods (close workflow, doc 10 §1)
		r.Get("/v1/books/{bookId}/periods", periodsSvc.HandleListPeriods)
		r.Post("/v1/books/{bookId}/periods", periodsSvc.HandleCreatePeriod)
		r.Post("/v1/books/{bookId}/periods/{periodId}/close", periodsSvc.HandleClosePeriod)
		r.Post("/v1/books/{bookId}/periods/{periodId}/reopen", periodsSvc.HandleReopenPeriod)

		// Document requests (doc 10 §4)
		r.Get("/v1/books/{bookId}/document-requests", periodsSvc.HandleListDocumentRequests)
		r.Post("/v1/books/{bookId}/document-requests", periodsSvc.HandleCreateDocumentRequest)
		r.Post("/v1/books/{bookId}/document-requests/{requestId}/waive", periodsSvc.HandleWaiveDocumentRequest)

		// Firm dashboard (doc 08 §6)
		r.Get("/v1/firm/dashboard", periodsSvc.HandleFirmDashboard)

		// Book settings (doc 07/08/09)
		r.Get("/v1/books/{bookId}/chart-of-accounts", settingsSvc.HandleListChartOfAccounts)
		r.Post("/v1/books/{bookId}/chart-of-accounts", settingsSvc.HandleCreateChartAccount)
		r.Patch("/v1/books/{bookId}/chart-of-accounts/{accountId}", settingsSvc.HandleUpdateChartAccount)
		r.Get("/v1/books/{bookId}/coa-templates", settingsSvc.HandleListCOATemplates)
		r.Post("/v1/books/{bookId}/chart-of-accounts/apply-template", settingsSvc.HandleApplyTemplate)
		r.Get("/v1/books/{bookId}/counterparty-aliases", settingsSvc.HandleListAliases)
		r.Post("/v1/books/{bookId}/counterparty-aliases", settingsSvc.HandleCreateAlias)
		r.Delete("/v1/books/{bookId}/counterparty-aliases/{aliasId}", settingsSvc.HandleDeleteAlias)
		r.Get("/v1/books/{bookId}/csv-mappings", settingsSvc.HandleListCSVMappings)
		r.Post("/v1/books/{bookId}/csv-mappings", settingsSvc.HandleCreateCSVMapping)
		r.Put("/v1/books/{bookId}/csv-mappings/{mappingId}", settingsSvc.HandleUpdateCSVMapping)

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
			r.Post("/rotate-keys", tenantSvc.HandleRotateKeys)

			// API keys (doc 07 §7)
			r.Get("/api-keys", settingsSvc.HandleListAPIKeys)
			r.Post("/api-keys", settingsSvc.HandleCreateAPIKey)
			r.Delete("/api-keys/{keyId}", settingsSvc.HandleRevokeAPIKey)

			// Webhooks (doc 07 §7)
			r.Get("/webhooks", settingsSvc.HandleListWebhooks)
			r.Post("/webhooks", settingsSvc.HandleCreateWebhook)
			r.Delete("/webhooks/{webhookId}", settingsSvc.HandleDeleteWebhook)
			r.Post("/webhooks/{webhookId}/test", settingsSvc.HandleTestWebhook)
		})

		// Mobile push device registration (doc 07 §8)
		r.Post("/v1/push/register", pushSvc.HandleRegisterDevice)
	})

	// Client portal — read-only, scoped to the portal user's own book
	r.Group(func(r chi.Router) {
		r.Use(portalSvc.RequirePortal)
		r.Get("/v1/portal/reports", portalSvc.HandleListReports)
		r.Get("/v1/portal/reports/{reportId}", portalSvc.HandleGetReport)
		r.Get("/v1/portal/findings", portalSvc.HandleListFindings)
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

// initTracer wires OpenTelemetry OTLP export to Jaeger (docker-compose jaeger service).
func initTracer(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "jaeger:4317"
	}
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName("ai-auditor-api")))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	return tp, nil
}