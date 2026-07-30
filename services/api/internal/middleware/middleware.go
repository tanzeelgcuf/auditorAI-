package middleware

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sony/gobreaker"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
)

type contextKey string

const (
	UserIDKey      contextKey = "user_id"
	FirmIDKey      contextKey = "firm_id"
	AssignedBooksKey contextKey = "assigned_books"
	RoleKey        contextKey = "role"
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
		"staff":       1,
		"firm_admin":  2,
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

// RLSInjector sets PostgreSQL session variables for Row Level Security
func RLSInjector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firmID := r.Context().Value(FirmIDKey)
		userID := r.Context().Value(UserIDKey)

		if firmID == nil || userID == nil {
			// Not authenticated yet — let Authenticator handle it
			next.ServeHTTP(w, r)
			return
		}

		// In a real app, fetch assigned books from DB (cached per-request)
		// For now, we'll use a placeholder - the actual implementation
		// would query user_book_assignments table
		assignedBooks := getAssignedBooks(r.Context(), firmID.(string), userID.(string))

		ctx := context.WithValue(r.Context(), FirmIDKey, firmID)
		ctx = context.WithValue(ctx, AssignedBooksKey, assignedBooks)

		// Set Postgres session variables via middleware that wraps the DB connection
		// This is handled by the database middleware that wraps each query

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetAssignedBooks retrieves the list of book IDs a user is assigned to
func GetAssignedBooks(ctx context.Context) []string {
	if books := ctx.Value(AssignedBooksKey); books != nil {
		return books.([]string)
	}
	return nil
}

// GetFirmID retrieves the firm ID from context
func GetFirmID(ctx context.Context) string {
	if id := ctx.Value(FirmIDKey); id != nil {
		return id.(string)
	}
	return ""
}

// GetUserID retrieves the user ID from context
func GetUserID(ctx context.Context) string {
	if id := ctx.Value(UserIDKey); id != nil {
		return id.(string)
	}
	return ""
}

// TraceInjector propagates OpenTelemetry trace context
func TraceInjector(next http.Handler) http.Handler {
	return middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenTelemetry propagation is handled by the otelhttp wrapper
		// This just ensures request ID is available
		next.ServeHTTP(w, r)
	}))
}

// CircuitBreaker wraps a downstream call with gobreaker
func CircuitBreaker(name string, settings gobreaker.Settings) func(http.Handler) http.Handler {
	cb := gobreaker.NewCircuitBreaker(settings)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := cb.Execute(func() (interface{}, error) {
				// Create a response recorder to capture the response
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
		// Rate limiting enforced at Traefik/Kong layer
		// This is a no-op placeholder for local dev
		next.ServeHTTP(w, r)
	})
}

// getAssignedBooks fetches assigned book IDs for a user
// In production, this should be cached per-request
func getAssignedBooks(ctx context.Context, firmID, userID string) []string {
	// TODO: Implement actual DB query to user_book_assignments
	// For firm_admin, return all books in the firm
	// For staff, return only assigned books
	return []string{} // placeholder
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
	w.WriteHeader(statusCode(status))
	// JSON encoding would go here
	_ = detail // suppress unused for now
}

func statusCode(status int) int {
	return status
}