package portal

// Client portal (doc 07 §5) — a firm's client logs in READ-ONLY to see their own
// book's audit_reports and audit_findings. Never extracted_entities or raw
// documents, no mutations.
//
// Scoping is now BELT AND BRACES: every handler still filters by explicit book
// id in SQL, and the connection those queries run on is primed with
// app.current_firm / app.assigned_books so the 30 RLS policies apply as well.
// Previously only the first of those was true — RequirePortal stashed an
// UNPRIMED pooled connection, which was survivable only because the API
// connected as the table owner and policies were never consulted. The moment the
// app moved to the non-owner auditor_app role, every portal query on an
// RLS-enabled table would have raised on the unset GUC: a total portal outage,
// with no test covering it.

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
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	// db is the RLS-ENFORCED pool (auditor_app). Everything after login runs here.
	db *pgxpool.Pool
	// sysDB is the BYPASSRLS pool (auditor_sys), needed for exactly two things:
	// the pre-auth login lookup (the caller has no firm scope yet — that is what
	// login determines) and resolving book -> firm so the GUCs can be set.
	sysDB   *pgxpool.Pool
	authSvc *auth.Service
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool)    { s.db = db }
func (s *Service) SetSysDB(db *pgxpool.Pool) { s.sysDB = db }
func (s *Service) SetAuth(a *auth.Service)   { s.authSvc = a }

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
	err := s.sysDB.QueryRow(r.Context(),
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

// RequirePortal validates a portal JWT, requires role=portal_user, and puts the
// scoped book id plus an RLS-PRIMED connection in the request context.
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

		// The portal token carries a BOOK id, but app.current_firm needs a FIRM id,
		// so resolve one from the other. This lookup has to run on sysDB: it is the
		// step that establishes the tenant scope, so it cannot itself be scoped.
		// It is a single indexed primary-key read.
		var firmID string
		if err := s.sysDB.QueryRow(r.Context(),
			"SELECT firm_id::text FROM client_books WHERE id = $1",
			claims.PortalBookID).Scan(&firmID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Book deleted since the token was issued. 403, not 404 — do not
				// confirm or deny the existence of an id to an unscoped caller.
				writeProblem(w, http.StatusForbidden, "https://ai-auditor.dev/errors/forbidden",
					"book scope no longer valid")
				return
			}
			slog.Error("portal firm lookup failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
				"internal error")
			return
		}

		// assigned_books is the portal user's ONE book. Any policy that consults
		// app.assigned_books therefore restricts this session to it, on top of the
		// explicit WHERE client_book_id = $1 each handler still applies.
		conn, ctx, err := middleware.AcquireScoped(r.Context(), s.db, firmID,
			[]string{claims.PortalBookID})
		if err != nil {
			slog.Error("portal scoped conn failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
				"no db conn")
			return
		}
		// ReleaseRLSConn, not conn.Release(): the GUCs must be scrubbed before this
		// connection can serve another tenant, and the scrub has to survive a
		// cancelled request context.
		defer middleware.ReleaseRLSConn(r.Context(), conn)

		ctx = context.WithValue(ctx, portalBookKey, claims.PortalBookID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getConn returns the RLS-primed connection AcquireScoped put in the request
// context. It delegates to middleware rather than keeping a private context key:
// the old private key was the reason the portal's connection could be left
// unprimed without anything noticing.
func getConn(ctx context.Context) *pgxpool.Conn {
	return middleware.GetConn(ctx)
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
