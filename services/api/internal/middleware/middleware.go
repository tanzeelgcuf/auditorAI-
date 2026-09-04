package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sony/gobreaker"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
)

// The four identity keys are DEFINED IN internal/auth (see auth/context.go) and
// only aliased here, because middleware imports auth and the reverse would be an
// import cycle. Do not redeclare them locally: a second definition of
// "user_id" under a second key type is exactly the bug that made both TOTP
// handlers return 401 for every caller until 2026-09-04.
const (
	UserIDKey        = auth.UserIDKey
	FirmIDKey        = auth.FirmIDKey
	AssignedBooksKey = auth.AssignedBooksKey
	RoleKey          = auth.RoleKey
)

// contextKey is for values that never cross a package boundary.
type contextKey string

const connKey contextKey = "rls_conn"

// Authenticator validates JWT and sets user context
func Authenticator(authSvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "Missing Authorization header")
				return
			}

			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			if tokenStr == authHeader {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "Invalid Authorization format")
				return
			}

			claims, err := authSvc.ValidateAccessToken(tokenStr)
			if err != nil {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "Invalid or expired token")
				return
			}

			ctx := context.WithValue(r.Context(), UserIDKey, claims.UserID)
			ctx = context.WithValue(ctx, FirmIDKey, claims.FirmID)
			ctx = context.WithValue(ctx, RoleKey, claims.Role)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole ensures user has at least the required role
func RequireRole(requiredRole string) func(http.Handler) http.Handler {
	roleHierarchy := map[string]int{
		"staff":      1,
		"firm_admin": 2,
	}

	requiredLevel := roleHierarchy[requiredRole]

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userRole := r.Context().Value(RoleKey)
			if userRole == nil {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "No role in context")
				return
			}

			userLevel := roleHierarchy[userRole.(string)]
			if userLevel < requiredLevel {
				writeProblem(w, r, "https://ai-auditor.dev/errors/forbidden", "Forbidden", http.StatusForbidden, "Insufficient role")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RLSInjector sets PostgreSQL session variables and stores assigned books in context.
// Acquires a dedicated DB connection for the request, sets app.current_firm and
// app.assigned_books, and puts the connection in context for handlers to use.
//
// IMPORTANT: set_config is called with is_local=false (the session-level default).
// The old `true` (is_local) form scoped the setting to the current transaction,
// which under autocommit (no explicit BEGIN) is discarded on commit — the very
// next statement ran without RLS. The session-level default persists for the
// life of the dedicated connection; a RESET on release scrubs the GUCs before
// the conn returns to the pool so no tenant context leaks into the next request.
//
// getAssignedBooks runs on the SAME dedicated connection, after current_firm is
// set. Pool connections may carry no GUC (current_setting errors for a
// non-superuser) or a LEAKED GUC from a prior request (cross-tenant rows); the
// request-scoped conn avoids both.
func RLSInjector(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			firmID := r.Context().Value(FirmIDKey)
			userID := r.Context().Value(UserIDKey)
			role := r.Context().Value(RoleKey)

			if firmID == nil || userID == nil || role == nil {
				next.ServeHTTP(w, r)
				return
			}

			firmIDStr := firmID.(string)
			userIDStr := userID.(string)
			roleStr := role.(string)

			conn, err := db.Acquire(r.Context())
			if err != nil {
				slog.Error("failed to acquire connection for RLS", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to acquire database connection")
				return
			}
			// ONE deferred cleanup for every exit path, including a panic. Before
			// this, RESET+Release were written inline after next.ServeHTTP, so a
			// panicking handler (caught upstream by Recoverer) leaked the pool
			// connection permanently — with pgx's default pool of 4*NumCPU, a
			// handful of panics exhausts the pool and the API stops serving.
			defer ReleaseRLSConn(r.Context(), conn)

			_, err = conn.Exec(r.Context(), "SELECT set_config('app.current_firm', $1, false)", firmIDStr)
			if err != nil {
				slog.Error("failed to set app.current_firm", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to set session context")
				return
			}

			assignedBooks, err := getAssignedBooks(r.Context(), conn, firmIDStr, userIDStr, roleStr)
			if err != nil {
				slog.Error("failed to get assigned books", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to load permissions")
				return
			}

			booksStr := strings.Join(assignedBooks, ",")
			_, err = conn.Exec(r.Context(), "SELECT set_config('app.assigned_books', $1, false)", booksStr)
			if err != nil {
				slog.Error("failed to set app.assigned_books", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to set session context")
				return
			}

			ctx := context.WithValue(r.Context(), AssignedBooksKey, assignedBooks)
			ctx = context.WithValue(ctx, connKey, conn)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AcquireScoped takes a connection from pool, sets app.current_firm and
// app.assigned_books on it, and returns it together with a context that
// GetConn can find it in.
//
// It exists for the one authenticated surface that does NOT go through
// RLSInjector: the client portal, whose tokens carry a book id instead of a firm
// id and whose middleware previously stashed an UNPRIMED connection under its own
// private context key. Unprimed meant every policy predicate on that connection
// raised on the unset GUC — invisible while the app connected as the owner,
// a total portal outage the moment it stopped.
//
// Callers must `defer ReleaseRLSConn(ctx, conn)` with the ORIGINAL request
// context.
func AcquireScoped(ctx context.Context, pool *pgxpool.Pool, firmID string, books []string) (*pgxpool.Conn, context.Context, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, ctx, err
	}
	if _, err := conn.Exec(ctx,
		"SELECT set_config('app.current_firm', $1, false)", firmID); err != nil {
		ReleaseRLSConn(ctx, conn)
		return nil, ctx, err
	}
	if _, err := conn.Exec(ctx,
		"SELECT set_config('app.assigned_books', $1, false)", strings.Join(books, ",")); err != nil {
		ReleaseRLSConn(ctx, conn)
		return nil, ctx, err
	}
	scoped := context.WithValue(ctx, AssignedBooksKey, books)
	scoped = context.WithValue(scoped, connKey, conn)
	return conn, scoped, nil
}

// ReleaseRLSConn scrubs the tenant GUCs off a request connection and returns it
// to the pool. If the scrub cannot be confirmed the connection is CLOSED instead
// of reused, because a pooled connection still carrying app.current_firm is a
// cross-tenant read waiting to happen.
func ReleaseRLSConn(reqCtx context.Context, conn *pgxpool.Conn) {
	// context.WithoutCancel is the load-bearing part. reqCtx is cancelled when
	// the client disconnects or chi's 30s Timeout fires, and Exec on a cancelled
	// context never reaches the server — so the previous inline
	// `conn.Exec(r.Context(), "RESET ...")` was a silent no-op on exactly the
	// requests most likely to be aborted mid-flight. The GUCs then survived into
	// the next request that acquired this connection.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), 5*time.Second)
	defer cancel()

	if _, err := conn.Exec(ctx, "RESET app.current_firm, app.assigned_books"); err != nil {
		slog.Error("failed to reset RLS session vars — closing connection instead of pooling it",
			"error", err)
		// pgxpool.Conn.Release() destroys the underlying resource rather than
		// returning it when the connection is already closed, so this is the
		// supported way to take a suspect connection out of circulation.
		_ = conn.Conn().Close(ctx)
	}
	conn.Release()
}

// Querier is the slice of pgx's API that request-scoped code uses. Both
// *pgxpool.Pool and *pgxpool.Conn implement it, which is what makes DB() below a
// drop-in replacement at a call site without changing any signature.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DB returns the RLS-wired request connection when there is one, otherwise the
// pool passed in.
//
// Why this exists: policies call current_setting('app.current_firm') with no
// missing_ok argument, so they RAISE on a connection that never had the GUC set.
// A handler that reaches for its own *pgxpool.Pool therefore does not "see all
// rows" once the app connects as auditor_app — it 500s. Both outcomes are wrong,
// and the pool one used to be invisible because the owner role ignored policies
// entirely.
//
// Handlers should call middleware.DB(ctx, s.db) instead of s.db. Background
// goroutines and pre-auth handlers have no request connection and must be given
// the BYPASSRLS sys pool explicitly rather than relying on this fallback.
func DB(ctx context.Context, fallback Querier) Querier {
	if c := GetConn(ctx); c != nil {
		return c
	}
	return fallback
}

// GetConn returns the RLS-wired DB connection from context, or nil.
// Handlers protected by RLSInjector should use this connection for queries.
func GetConn(ctx context.Context) *pgxpool.Conn {
	if c, ok := ctx.Value(connKey).(*pgxpool.Conn); ok {
		return c
	}
	return nil
}

// GetAssignedBooks retrieves the list of book IDs from context.
func GetAssignedBooks(ctx context.Context) []string {
	if books := ctx.Value(AssignedBooksKey); books != nil {
		return books.([]string)
	}
	return nil
}

// GetFirmID retrieves the firm ID from context.
func GetFirmID(ctx context.Context) string {
	if id := ctx.Value(FirmIDKey); id != nil {
		return id.(string)
	}
	return ""
}

// GetUserID retrieves the user ID from context.
func GetUserID(ctx context.Context) string {
	if id := ctx.Value(UserIDKey); id != nil {
		return id.(string)
	}
	return ""
}

// GetRole retrieves the user's role from context.
func GetRole(ctx context.Context) string {
	if r := ctx.Value(RoleKey); r != nil {
		return r.(string)
	}
	return ""
}

// TraceInjector propagates OpenTelemetry trace context
func TraceInjector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// CircuitBreaker wraps a downstream call with gobreaker
func CircuitBreaker(name string, settings gobreaker.Settings) func(http.Handler) http.Handler {
	cb := gobreaker.NewCircuitBreaker(settings)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := cb.Execute(func() (interface{}, error) {
				rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
				next.ServeHTTP(rec, r)
				if rec.statusCode >= 500 {
					return nil, errors.New("downstream error")
				}
				return nil, nil
			})

			if err != nil {
				writeProblem(w, r, "https://ai-auditor.dev/errors/service-unavailable", "Service Unavailable", http.StatusServiceUnavailable, "Downstream service temporarily unavailable")
				return
			}
		})
	}
}

// getAssignedBooks fetches assigned book UUIDs for a user.
// firm_admin gets ALL books in their firm; staff gets only user_book_assignments.
// Runs on the RLS-wired request connection so the firm GUC scopes the reads.
func getAssignedBooks(ctx context.Context, db queryer, firmID, userID, role string) ([]string, error) {
	if role == "firm_admin" {
		rows, err := db.Query(ctx, "SELECT id::text FROM client_books WHERE firm_id = $1", firmID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var books []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			books = append(books, id)
		}
		return books, rows.Err()
	}

	// staff: only assigned books
	rows, err := db.Query(ctx,
		"SELECT client_book_id::text FROM user_book_assignments WHERE user_id = $1", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		books = append(books, id)
	}
	return books, rows.Err()
}

// queryer abstracts the two connection types getAssignedBooks runs against.
type queryer interface {
	Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error)
}

// responseRecorder captures status code
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// writeProblem writes an RFC 7807 problem+json response
func writeProblem(w http.ResponseWriter, r *http.Request, typ, title string, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":     typ,
		"title":    title,
		"status":   status,
		"detail":   detail,
		"instance": r.URL.Path,
	})
}
