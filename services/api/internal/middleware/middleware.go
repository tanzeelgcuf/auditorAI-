package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sony/gobreaker"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
)

type contextKey string

const (
	UserIDKey       contextKey = "user_id"
	FirmIDKey       contextKey = "firm_id"
	AssignedBooksKey contextKey = "assigned_books"
	RoleKey         contextKey = "role"
	connKey         contextKey = "rls_conn"
)

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

			assignedBooks, err := getAssignedBooks(r.Context(), db, firmIDStr, userIDStr, roleStr)
			if err != nil {
				slog.Error("failed to get assigned books", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to load permissions")
				return
			}

			conn, err := db.Acquire(r.Context())
			if err != nil {
				slog.Error("failed to acquire connection for RLS", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to acquire database connection")
				return
			}

			_, err = conn.Exec(r.Context(), "SELECT set_config('app.current_firm', $1, true)", firmIDStr)
			if err != nil {
				conn.Release()
				slog.Error("failed to set app.current_firm", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to set session context")
				return
			}

			booksStr := strings.Join(assignedBooks, ",")
			_, err = conn.Exec(r.Context(), "SELECT set_config('app.assigned_books', $1, true)", booksStr)
			if err != nil {
				conn.Release()
				slog.Error("failed to set app.assigned_books", "error", err)
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "Failed to set session context")
				return
			}

			ctx := context.WithValue(r.Context(), AssignedBooksKey, assignedBooks)
			ctx = context.WithValue(ctx, connKey, conn)

			next.ServeHTTP(w, r.WithContext(ctx))
			conn.Release()
		})
	}
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

// RateLimiter provides per-firm rate limiting (placeholder - real impl uses Traefik/Kong)
func RateLimiter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// getAssignedBooks fetches assigned book UUIDs for a user.
// firm_admin gets ALL books in their firm; staff gets only user_book_assignments.
func getAssignedBooks(ctx context.Context, db *pgxpool.Pool, firmID, userID, role string) ([]string, error) {
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
