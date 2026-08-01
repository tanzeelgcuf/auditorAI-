package portal

// Client portal (doc 07 §5) — a firm's client logs in READ-ONLY to see their own
// book's audit_reports and audit_findings. Never extracted_entities or raw
// documents, no mutations. Scoped by explicit book id (not RLS session vars).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
)

type Service struct {
	db     *pgxpool.Pool
	authSvc *auth.Service
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool)     { s.db = db }
func (s *Service) SetAuth(a *auth.Service)    { s.authSvc = a }

type ctxKey string

const portalBookKey ctxKey = "portal_book_id"

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// HandleLogin validates an invite token and issues a portal-scoped JWT.
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email       string `json:"email"`
		InviteToken string `json:"invite_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" || req.InviteToken == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"email and invite_token required")
		return
	}

	var id, bookID string
	var storedToken *string
	var expires *time.Time
	err := s.db.QueryRow(r.Context(),
		`SELECT id::text, client_book_id::text, invite_token, invite_expires
		 FROM client_portal_users WHERE email = $1`, req.Email).
		Scan(&id, &bookID, &storedToken, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized",
				"invalid invite")
			return
		}
		slog.Error("portal login query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"internal error")
		return
	}
	if storedToken == nil || *storedToken != req.InviteToken {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized",
			"invalid invite")
		return
	}
	if expires != nil && expires.Before(time.Now()) {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized",
			"invite expired")
		return
	}

	pair, err := s.authSvc.GeneratePortalTokens(id, bookID)
	if err != nil {
		slog.Error("portal token generation failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": pair.AccessToken, "refresh_token": pair.RefreshToken,
	})
}

// RequirePortal validates a portal JWT, requires role=portal_user, and stores the
// scoped book id + a pooled conn (no RLS session vars — handlers filter by book id).
func (s *Service) RequirePortal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if len(authHeader) < 8 || authHeader[:7] != "Bearer " {
			writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized",
				"missing bearer token")
			return
		}
		claims, err := s.authSvc.ValidateAccessToken(authHeader[7:])
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized",
				"invalid or expired token")
			return
		}
		if claims.Role != "portal_user" {
			writeProblem(w, http.StatusForbidden, "https://ai-auditor.dev/errors/forbidden",
				"portal access only")
			return
		}
		if claims.PortalBookID == "" {
			writeProblem(w, http.StatusForbidden, "https://ai-auditor.dev/errors/forbidden",
				"no book scope on token")
			return
		}

		conn, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
				"no db conn")
			return
		}
		defer conn.Release()

		ctx := context.WithValue(r.Context(), portalBookKey, claims.PortalBookID)
		ctx = context.WithValue(ctx, connKey, conn)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type connKeyT string

const connKey connKeyT = "portal_conn"

func getConn(ctx context.Context) *pgxpool.Conn {
	if c, ok := ctx.Value(connKey).(*pgxpool.Conn); ok {
		return c
	}
	return nil
}

// GetPortalBookID returns the portal user's scoped book id from context.
func GetPortalBookID(ctx context.Context) string {
	if b, ok := ctx.Value(portalBookKey).(string); ok {
		return b
	}
	return ""
}

// bookGuard returns false and writes a 404 if the resource's book != the caller's
// scoped book (no existence leak).
func bookGuard(w http.ResponseWriter, ctx context.Context, resourceBookID string) bool {
	if resourceBookID != GetPortalBookID(ctx) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found",
			"resource not found")
		return false
	}
	return true
}

// HandleListReports lists the portal user's own book's reports.
func (s *Service) HandleListReports(w http.ResponseWriter, r *http.Request) {
	bookID := GetPortalBookID(r.Context())
	c := getConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT id::text, to_char(period_start,'YYYY-MM-DD'), to_char(period_end,'YYYY-MM-DD'),
			to_char(generated_at,'YYYY-MM-DD"T"HH24:MI:SS'), cardinality(finding_ids)
		 FROM audit_reports WHERE client_book_id = $1 ORDER BY generated_at DESC LIMIT 50`, bookID)
	if err != nil {
		slog.Error("portal reports query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type report struct {
		ID            string `json:"id"`
		PeriodStart   string `json:"period_start"`
		PeriodEnd     string `json:"period_end"`
		GeneratedAt   string `json:"generated_at"`
		FindingCount  int    `json:"finding_count"`
	}
	var out []report
	for rows.Next() {
		var rp report
		if err := rows.Scan(&rp.ID, &rp.PeriodStart, &rp.PeriodEnd, &rp.GeneratedAt, &rp.FindingCount); err != nil {
			continue
		}
		out = append(out, rp)
	}
	if out == nil {
		out = []report{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

// HandleGetReport returns one report only if it belongs to the scoped book.
func (s *Service) HandleGetReport(w http.ResponseWriter, r *http.Request) {
	reportID := r.PathValue("reportId")
	if reportID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "reportId required")
		return
	}
	c := getConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var id, bookID, start, end, genAt string
	var findingIDs []string
	err := c.QueryRow(r.Context(),
		`SELECT id::text, client_book_id::text, to_char(period_start,'YYYY-MM-DD'),
			to_char(period_end,'YYYY-MM-DD'), to_char(generated_at,'YYYY-MM-DD"T"HH24:MI:SS'), finding_ids
		 FROM audit_reports WHERE id = $1`, reportID).
		Scan(&id, &bookID, &start, &end, &genAt, &findingIDs)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "report not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	if !bookGuard(w, r.Context(), bookID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "period_start": start, "period_end": end, "generated_at": genAt, "finding_ids": findingIDs,
	})
}

// HandleListFindings lists findings for the scoped book (read-only).
func (s *Service) HandleListFindings(w http.ResponseWriter, r *http.Request) {
	bookID := GetPortalBookID(r.Context())
	c := getConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT id::text, severity, status, rule_id, calculation_formula, created_at
		 FROM audit_findings WHERE client_book_id = $1 ORDER BY created_at DESC LIMIT 100`, bookID)
	if err != nil {
		slog.Error("portal findings query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type finding struct {
		ID            string    `json:"id"`
		Severity      string    `json:"severity"`
		Status        string    `json:"status"`
		RuleID        string    `json:"rule_id"`
		Formula       string    `json:"calculation_formula"`
		CreatedAt     time.Time `json:"created_at"`
	}
	var out []finding
	for rows.Next() {
		var f finding
		if err := rows.Scan(&f.ID, &f.Severity, &f.Status, &f.RuleID, &f.Formula, &f.CreatedAt); err != nil {
			continue
		}
		out = append(out, f)
	}
	if out == nil {
		out = []finding{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}
