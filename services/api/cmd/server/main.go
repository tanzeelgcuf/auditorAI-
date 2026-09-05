package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
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

	// Initialize Sentry (GlitchTip, Sentry-compatible). Strict no-op unless
	// GLITCHTIP_DSN is set; server must start identically to today.
	initSentry()

	// Initialize database pools. TWO of them, deliberately.
	//
	// pool    -> DATABASE_URL,     role auditor_app: NOSUPERUSER, NOBYPASSRLS,
	//            not the table owner. Every RLS policy in infra/init.sql applies.
	//            This is the only pool a request handler may touch.
	// sysPool -> SYS_DATABASE_URL, role auditor_sys: NOSUPERUSER but BYPASSRLS.
	//            For work that has no single firm to scope to: pre-auth identity
	//            lookups (login must find firm_id FROM the email; signup creates
	//            the firm row a policy would need to already exist) and the
	//            cross-firm background sweepers.
	//
	// Splitting these is what makes the 30 policies real. While the API connected
	// as `auditor` — initdb's bootstrap SUPERUSER and the owner of every table —
	// Postgres exempted it from row security unconditionally, so all 30 policies
	// were decoration. See the long comment above the roles section of
	// infra/init.sql.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://auditor_app:auditor@localhost:5432/ai_auditor?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// No silent fallback to `pool`. If SYS_DATABASE_URL is missing, login,
	// signup, password reset, the portal and all three background workers would
	// run as auditor_app with no app.current_firm set — every policy predicate
	// raises on the unset GUC, so the failure would be a wall of 500s at the
	// worst possible moment rather than a clear message now.
	sysDSN := os.Getenv("SYS_DATABASE_URL")
	if sysDSN == "" {
		slog.Error("SYS_DATABASE_URL is not set",
			"detail", "the API needs a second DSN for the BYPASSRLS role auditor_sys; "+
				"see .env.example. Pre-auth lookups and background workers cannot run on "+
				"the RLS-enforced pool because they are what determines the firm id.")
		os.Exit(1)
	}
	sysPool, err := pgxpool.New(ctx, sysDSN)
	if err != nil {
		slog.Error("failed to connect to database as auditor_sys", "error", err)
		os.Exit(1)
	}
	defer sysPool.Close()

	// Refuse to serve if the roles are not what we think they are. This is an
	// isolation test, not a config read: it asks Postgres what it actually
	// enforces for these two connections.
	if err := assertRolePosture(ctx, pool, sysPool); err != nil {
		slog.Error("database role posture check failed — refusing to start", "error", err)
		os.Exit(1)
	}

	// Seed chart-of-accounts templates (idempotent; warn-only on failure).
	// sysPool: this runs at startup with no request behind it, so there is no
	// app.current_firm to satisfy a policy predicate.
	if err := settings.SeedTemplates(ctx, sysPool); err != nil {
		slog.Warn("failed to seed COA templates", "error", err)
	}

	// Proactive stale document-request reminder loop. Sweeps every
	// firm, so it cannot be scoped to one — sysPool.
	go notify.Run(ctx, sysPool, notify.DefaultInterval)

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
	//
	// WHICH POOL: a service gets sysPool when the work it does cannot be scoped to
	// one firm, and pool (RLS-enforced) otherwise.
	//
	// auth is the clearest sysPool case. Every /v1/auth route is mounted PUBLIC —
	// there is no JWT yet, so RLSInjector has not run and no app.current_firm
	// exists. Worse, the work is inherently cross-firm: login has to find firm_id
	// FROM the submitted email, and signup INSERTs the firms row that a policy
	// predicate would need to already exist. On the RLS pool every one of those
	// statements raises on the unset GUC.
	authSvc := auth.NewService()
	authSvc.SetDB(sysPool)
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
	// extracted_entities -> entity.extraction.requested.
	if pipelineClient != nil && st != nil {
		if ingURL := os.Getenv("INGESTION_GRPC_ADDR"); ingURL != "" {
			coord, err := pipeline.NewCoordinator(os.Getenv("NATS_URL"), ingURL, sysPool, st)
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

	// Verification worker: consumes verification.requested -> loads group
	// totals -> Rust gRPC -> writes audit_findings (Prompt 3 wiring).
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		if vURL := os.Getenv("VERIFICATION_GRPC_ADDR"); vURL != "" {
			vw, err := pipeline.NewVerifyWorker(natsURL, vURL, sysPool)
			if err != nil {
				slog.Warn("verify worker unavailable", "error", err)
			} else {
				go func() {
					if err := vw.Run(ctx); err != nil {
						slog.Error("verify worker stopped", "error", err)
					}
				}()
			}
		}
	}

	entitySvc := entities.NewService()
	entitySvc.SetDB(pool)
	findingSvc := findings.NewService()
	findingSvc.SetDB(pool)
	// HandleAddAttachment takes a storage_key from the client and must verify the
	// object exists before recording it, so findings needs the same storage client
	// docSvc got. `st` is nil when storage.New() failed above; the handler answers
	// 503 in that case rather than recording an unverifiable key.
	findingSvc.SetStorage(st)
	reviewSvc := review.NewService()
	reviewSvc.SetDB(pool)
	billingSvc := billing.NewService()

	periodsSvc := periods.NewService()
	periodsSvc.SetDB(pool)

	// settings takes no pool: every statement in it runs on the RLS-primed request
	// connection, and its one pre-scope function (settings.AuthAPIKey, currently
	// unwired) takes sysPool as an explicit parameter. The SetDB(pool) that used to
	// be here fed a field that became write-only when conn() stopped falling back
	// to Acquire().
	settingsSvc := settings.NewService()
	billingSvc.SetDB(pool)
	// The Stripe webhook is a cross-firm actor with no JWT, so it cannot satisfy the
	// `firms` RLS policy (init.sql:466 -> id = current_setting('app.current_firm')).
	// Worse, that policy omits the missing_ok argument, so on a request with no
	// app.current_firm set current_setting RAISES rather than returning NULL — the
	// UPDATE would error, not merely match zero rows. sysPool (BYPASSRLS) is the
	// established pattern for exactly this: see authSvc and webhooksSvc above.
	// HandleCheckout keeps using the RLS-bound pool; only the webhook uses this one.
	billingSvc.SetSysDB(sysPool)
	mcpSvc := mcp.NewService()
	// No mcpSvc.SetDB: the service holds no pool. All four /mcp/tools/* handlers
	// run on the connection InternalAuth primed via AcquireScoped; the field this
	// call used to set existed only to feed an Acquire() fallback, which returned
	// an UNPRIMED connection from this same RLS-enforced pool and so could not
	// have served any of the queries it was reached for.

	if pipelineClient != nil {
		mcpSvc.SetVerificationPublisher(pipelineClient)
	}

	// webhooksSvc has no HTTP routes — it is reached only through
	// findingSvc.Notifier. Its DB work is interleaved with outbound HTTP delivery
	// and retry backoffs (recordFailure runs AFTER the retries), so it can outlive
	// the request that triggered it. Handing it the request connection would be a
	// use-after-release; it gets sysPool and keeps its own `WHERE firm_id = $1`.
	webhooksSvc := webhooks.NewService()
	webhooksSvc.SetDB(sysPool)
	findingSvc.Notifier = webhooksSvc

	// Portal needs both: sysPool for the pre-auth invite lookup and the
	// book -> firm resolution, pool for everything a logged-in portal user reads.
	portalSvc := portal.NewService()
	portalSvc.SetDB(pool)
	portalSvc.SetSysDB(sysPool)
	portalSvc.SetAuth(authSvc)

	// push takes no pool either, for the same reason, and for a second one: its
	// background fan-out (push.SendFindingAlert) needs sysPool while its handler
	// needs the primed request connection. One field could only ever have been right
	// for one of them, and SetDB(pool) made it the wrong one for the fan-out.
	pushSvc := push.NewService()

	humanSvc := humanoverride.NewService()
	humanSvc.SetDB(pool)

	// Router
	//
	// TRUSTED PROXIES. chimiddleware.RealIP used to sit here. It rewrites
	// r.RemoteAddr from True-Client-IP / X-Real-IP / X-Forwarded-For with no
	// notion of a trusted proxy, and the old ratelimit.clientIP then preferred
	// the LEFTMOST X-Forwarded-For element over RemoteAddr — the element the
	// client writes, since proxies append on the right. Result: one header made
	// every per-IP limit in this service unbounded, including the six-digit guess
	// surface at /v1/totp/verify. This compose file publishes api as 8080:8080
	// and no service carries traefik labels, so that header arrived untouched.
	//
	// middleware.RealIP(trustedProxies) does nothing unless the peer is one of
	// TRUSTED_PROXY_CIDRS, and then walks XFF right-to-left. Empty set = headers
	// ignored, which fails closed: clients behind an unconfigured proxy share a
	// bucket instead of each getting a private unlimited one.
	trustedProxies, badProxyEntries := middleware.ParseTrustedProxies(os.Getenv("TRUSTED_PROXY_CIDRS"))
	middleware.LogTrustedProxies(trustedProxies, badProxyEntries, os.Getenv("APP_ENV"))

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.RealIP(trustedProxies))
	// MUST stay directly below RealIP. SourceIP copies the (by now rewritten)
	// RemoteAddr onto the context for the audit writers, so above RealIP it would
	// record the proxy instead of the client, silently and forever. It is also the
	// reason access_log.source_ip cannot disagree with the address the rate
	// limiter bucketed: both read the same single resolution.
	r.Use(middleware.SourceIP)
	r.Use(middleware.SentryRecoverer) // must run BEFORE Recoverer to capture panics
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

	// Rate limiters (per-IP token bucket). Auth and uploads are
	// the brute-force / abuse surfaces; admin key operations are low-traffic.
	// Rates: auth 5 req/s burst 20; upload 3 req/s burst 10; admin 2 req/s burst 5.
	authLimiter := middleware.NewIPRateLimiter(5, 20)
	uploadLimiter := middleware.NewIPRateLimiter(3, 10)
	adminLimiter := middleware.NewIPRateLimiter(2, 5)

	// Client portal login (public — invite-token based)
	r.With(middleware.RateLimit(authLimiter)).Post("/v1/portal/login", portalSvc.HandleLogin)

	// Stripe webhook (public by necessity, 2026-09-04).
	//
	// This route USED TO BE registered inside the `r.Group` below that applies
	// middleware.Authenticator. Stripe cannot present a JWT, so every delivery was
	// rejected with 401 before the handler ran: subscription created/updated/deleted
	// and payment-failure events never reached the application, and billing state
	// diverged from Stripe silently.
	//
	// Public here does NOT mean unauthenticated. HandleStripeWebhook authenticates
	// the caller the way Stripe intends and refuses to fail open:
	//   - STRIPE_WEBHOOK_SECRET unset -> 503, no processing at all
	//   - webhook.ConstructEvent verifies the Stripe-Signature HMAC over the raw
	//     body -> 400 on mismatch
	// That is a stronger check for this caller than a JWT group could ever be, since
	// no JWT exists to check.
	//
	// Rate-limited with authLimiter because an unauthenticated POST that reads a
	// request body is an abuse surface; the handler also caps the body it will read.
	// Stripe's own delivery volume is far below 5 req/s.
	r.With(middleware.RateLimit(authLimiter)).Post("/v1/webhooks/stripe", billingSvc.HandleStripeWebhook)

	// Auth routes (public) — rate-limited against brute force.
	r.Route("/v1/auth", func(r chi.Router) {
		r.Use(middleware.RateLimit(authLimiter))
		r.Post("/signup", authSvc.HandleSignup)
		r.Post("/login", authSvc.HandleLogin)
		r.Post("/logout", authSvc.HandleLogout)
		r.Post("/refresh", authSvc.HandleRefresh)
		r.Get("/verify-email", authSvc.HandleVerifyEmail)
		r.Post("/forgot-password", authSvc.HandleForgotPassword)
		r.Post("/reset-password", authSvc.HandleResetPassword)
		// NOTE: /totp/enable and /totp/verify used to be registered HERE, inside
		// this public group. They read the caller's identity from the request
		// context, which only middleware.Authenticator populates, so in the public
		// group they had no identity to read and returned 401 to everyone. They now
		// live at /v1/totp/* in the authenticated group below.
	})

	// Second-factor enrollment (2026-09-04) — authenticated, but deliberately NOT
	// inside the big protected group below, because these two handlers query the
	// `users` table through the auth service's own sysPool and never touch the
	// RLS-scoped pool; mounting them there would make RLSInjector check out a
	// connection and set GUCs on it for the whole request for no reason.
	//
	// Identity comes from the verified JWT (auth.UserIDFrom), and every statement
	// is scoped `WHERE id = <that user>`, so BYPASSRLS here does not widen access:
	// a caller can only ever enroll a factor on their own account.
	//
	// Rate-limited with authLimiter: /totp/verify is a 6-digit guess surface, and
	// without a limiter 10^6 is a small number.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticator(authSvc))
		r.Use(middleware.RateLimit(authLimiter))
		r.Post("/v1/totp/enable", authSvc.HandleEnableTOTP)
		r.Post("/v1/totp/verify", authSvc.HandleVerifyTOTP)
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.Authenticator(authSvc))
		// RLSInjector must run AFTER Authenticator sets firm/user/role in context.
		r.Use(middleware.RLSInjector(pool))

		// Tenant/Book management
		//
		// The three write routes below are firm_admin-only, and were NOT before
		// 2026-09-06. tenant.go carried two comments asserting the restriction —
		// "Only firm_admin can assign staff — enforced by RequireRole middleware at
		// /v1/admin" and "Only firm_admin can reach this route anyway (RequireRole on
		// the /v1/admin group)" — but the routes are mounted HERE, in the general
		// protected group, and the only RequireRole in this file is at the /v1/admin
		// group below. The comments described an intention; the router applied none
		// of it.
		//
		// What that cost: user_book_assignments is governed by
		// assignments_own_firm_only (init.sql:606), which gates on FIRM, not on
		// app.assigned_books. So for this one table the Go layer was the only
		// book-level control, and HandleAssignStaff never checked bookId against the
		// caller's assignments either. Any staff JWT could POST
		// /v1/books/{anyBookInTheirFirm}/staff with their own user_id, and on the very
		// next request RLSInjector recomputes app.assigned_books from that table —
		// defeating the second level of the two-level RLS model outright and exposing
		// every document, entity, group, finding and report of every client of the
		// firm. HandleRemoveStaff was the mirror image: any staff could unassign the
		// firm admin from any book.
		//
		// Kept at these paths rather than moved under /v1/admin: nothing in apps/web
		// calls them (grep for "/staff" finds only the /admin/team nav link), so the
		// path is free to move, but the role belongs on the route and moving URLs
		// would make the fix look like a refactor in the diff. The handlers enforce
		// firm membership of the target user independently — see tenant.go.
		r.Route("/v1/books", func(r chi.Router) {
			r.Get("/", tenantSvc.HandleListBooks)
			r.With(middleware.RequireRole("firm_admin")).Post("/", tenantSvc.HandleCreateBook)
			r.Get("/{bookId}", tenantSvc.HandleGetBook)
			r.Patch("/{bookId}/settings", tenantSvc.HandleUpdateBookSettings)
			r.With(middleware.RequireRole("firm_admin")).Post("/{bookId}/staff", tenantSvc.HandleAssignStaff)
			r.With(middleware.RequireRole("firm_admin")).Delete("/{bookId}/staff/{userId}", tenantSvc.HandleRemoveStaff)
		})

		// Documents — upload-url is a storage-abuse surface (presigned PUTs),
		// rate-limited separately from the authenticated list/get.
		r.Route("/v1/books/{bookId}/documents", func(r chi.Router) {
			r.With(middleware.Idempotency(pool)).Post("/", docSvc.HandleUpload)
			r.With(middleware.RateLimit(uploadLimiter)).Post("/upload-url", docSvc.HandlePresignUpload)
			r.Get("/", docSvc.HandleList)
			r.Get("/{docId}", docSvc.HandleGet)
			r.Get("/{docId}/view", docSvc.HandlePresignedView)
			r.Post("/{docId}/confirm-upload", docSvc.HandleConfirmUpload)
		})

		// Entities
		r.Get("/v1/books/{bookId}/entities", entitySvc.HandleList)

		// Human override — manual entity creation + group split/merge
		r.Post("/v1/books/{bookId}/entities/manual", humanSvc.HandleCreateManualEntity)
		r.Post("/v1/reconciliation-groups/{groupId}/split", humanSvc.HandleSplitGroup)
		r.Post("/v1/reconciliation-groups/merge", humanSvc.HandleMergeGroups)

		// Config change history
		r.Get("/v1/books/{bookId}/config-history", humanSvc.HandleConfigHistory)

		// Automation rate
		r.Get("/v1/books/{bookId}/automation-rate", humanSvc.HandleAutomationRate)

		// Tags
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
		// NOTE: /v1/webhooks/stripe is deliberately NOT here. It was, and that was
		// the bug — see the public registration above. HandleCheckout stays in this
		// group because it legitimately has a JWT and must be RLS-scoped.

		// Periods (close workflow)
		r.Get("/v1/books/{bookId}/periods", periodsSvc.HandleListPeriods)
		r.Post("/v1/books/{bookId}/periods", periodsSvc.HandleCreatePeriod)
		r.Post("/v1/books/{bookId}/periods/{periodId}/close", periodsSvc.HandleClosePeriod)
		r.Post("/v1/books/{bookId}/periods/{periodId}/reopen", periodsSvc.HandleReopenPeriod)

		// Document requests
		r.Get("/v1/books/{bookId}/document-requests", periodsSvc.HandleListDocumentRequests)
		r.Post("/v1/books/{bookId}/document-requests", periodsSvc.HandleCreateDocumentRequest)
		r.Post("/v1/books/{bookId}/document-requests/{requestId}/waive", periodsSvc.HandleWaiveDocumentRequest)

		// Firm dashboard
		r.Get("/v1/firm/dashboard", periodsSvc.HandleFirmDashboard)

		// Book settings
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

		// Firm admin
		r.Route("/v1/admin", func(r chi.Router) {
			r.Use(middleware.RequireRole("firm_admin"))
			r.Use(middleware.RateLimit(adminLimiter)) // low-traffic admin ops
			r.Get("/team", tenantSvc.HandleListStaff)
			r.Get("/settings", tenantSvc.HandleGetFirmSettings)
			r.Patch("/settings", tenantSvc.HandleUpdateFirmSettings)
			r.Post("/rotate-keys", tenantSvc.HandleRotateKeys)

			// API keys
			r.Get("/api-keys", settingsSvc.HandleListAPIKeys)
			r.Post("/api-keys", settingsSvc.HandleCreateAPIKey)
			r.Delete("/api-keys/{keyId}", settingsSvc.HandleRevokeAPIKey)

			// Webhooks
			r.Get("/webhooks", settingsSvc.HandleListWebhooks)
			r.Post("/webhooks", settingsSvc.HandleCreateWebhook)
			r.Delete("/webhooks/{webhookId}", settingsSvc.HandleDeleteWebhook)
			r.Post("/webhooks/{webhookId}/test", settingsSvc.HandleTestWebhook)
		})

		// Mobile push device registration
		r.Post("/v1/push/register", pushSvc.HandleRegisterDevice)
	})

	// MCP tools (internal, called by agent-runtime). Outside the user-auth
	// group: authenticated with the shared internal key instead of a user JWT,
	// scoping to the client_book_id in the request body.
	//
	// Both pools: sysPool resolves book -> firm (the step that establishes scope,
	// so it cannot itself be scoped), then the handlers run on an RLS-primed
	// connection from pool.
	r.Group(func(r chi.Router) {
		r.Use(middleware.InternalAuth(pool, sysPool))
		r.Post("/mcp/tools/get_pending_entities", mcpSvc.HandleGetPendingEntities)
		r.Post("/mcp/tools/create_entity_link", mcpSvc.HandleCreateEntityLink)
		r.Post("/mcp/tools/flag_for_review", mcpSvc.HandleFlagForReview)
		r.Post("/mcp/tools/get_book_tolerance", mcpSvc.HandleGetBookTolerance)
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
		sentry.Flush(2 * time.Second) // send queued error events before exit
	}()

	slog.Info("server starting", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// initSentry wires error reporting to GlitchTip (Sentry-compatible) when
// GLITCHTIP_DSN is set; otherwise it is a strict no-op.
func initSentry() {
	dsn := os.Getenv("GLITCHTIP_DSN")
	if dsn == "" {
		return
	}
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "dev"
	}
	release := "dev"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				release = s.Value[:7]
				break
			}
		}
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      env,
		Release:          release,
		TracesSampleRate: 0.0, // errors only; tracing stays in OTel
	}); err != nil {
		slog.Warn("sentry init failed — error reporting disabled", "error", err)
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