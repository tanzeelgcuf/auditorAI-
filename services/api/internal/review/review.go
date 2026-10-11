package review

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

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

// primed returns the RLS-primed request connection, or false after logging and
// writing a 500.
//
// No Acquire() fallback: reviewSvc.SetDB(pool) at main.go:219 is the
// RLS-ENFORCED app pool, so the connection Acquire() returned had no
// app.current_firm set, and reconciliation_groups /
// reconciliation_group_members / extracted_entities all carry policies that cast
// current_setting(...) to uuid with no missing_ok — the statement raises rather
// than filtering.
//
// The pool field stays, unlike mcp.go's: RecordAccess (review.go:167) and the
// middleware.DB call at :189 both still need it.
//
// OBSERVED: unreachable today. All four review routes (main.go:436-439) are in
// the group that does r.Use(middleware.RLSInjector(pool)), so GetConn is
// non-nil. REASONED from the route table; not executed.
func (s *Service) primed(w http.ResponseWriter, r *http.Request) (middleware.Querier, bool) {
	if c := middleware.GetConn(r.Context()); c != nil {
		return c, true
	}
	slog.Error("review: no RLS-primed connection; route is mounted outside the "+
		"RLSInjector group and every statement here would raise",
		"path", r.URL.Path, "method", r.Method)
	writeProblem(w, http.StatusInternalServerError,
		"https://ai-auditor.dev/errors/internal", "no db conn")
	return nil, false
}

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" {
		status = "needs_review"
	}

	c, ok := s.primed(w, r)
	if !ok {
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT rg.id::text, rg.client_book_id::text, rg.link_confidence, rg.status, rg.created_at,
			COALESCE((SELECT e.id::text FROM reconciliation_group_members m
				JOIN extracted_entities e ON e.id = m.extracted_entity_id
				WHERE m.reconciliation_group_id = rg.id AND m.role = 'invoice' LIMIT 1), ''),
			COALESCE((SELECT e.id::text FROM reconciliation_group_members m
				JOIN extracted_entities e ON e.id = m.extracted_entity_id
				WHERE m.reconciliation_group_id = rg.id AND m.role = 'bank' LIMIT 1), ''),
			COALESCE((SELECT e.id::text FROM reconciliation_group_members m
				JOIN extracted_entities e ON e.id = m.extracted_entity_id
				WHERE m.reconciliation_group_id = rg.id AND m.role = 'gl' LIMIT 1), '')
		 FROM reconciliation_groups rg
		 WHERE rg.client_book_id = $1 AND rg.status = $2
		 ORDER BY rg.created_at DESC LIMIT 100`, bookID, status)
	if err != nil {
		slog.Error("failed to list review queue", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type item struct {
		ID              string    `json:"id"`
		ClientBookID    string    `json:"client_book_id"`
		InvoiceEntityID *string   `json:"invoice_entity_id"`
		BankEntityID    *string   `json:"bank_entity_id"`
		GLEntityID      *string   `json:"gl_entity_id"`
		LinkConfidence  float64   `json:"link_confidence"`
		Status          string    `json:"status"`
		CreatedAt       time.Time `json:"created_at"`
	}

	var out []item
	for rows.Next() {
		var it item
		var createdAt time.Time
		var invID, bankID, glID string
		if err := rows.Scan(&it.ID, &it.ClientBookID, &it.LinkConfidence, &it.Status, &createdAt,
			&invID, &bankID, &glID); err != nil {
			continue
		}
		it.InvoiceEntityID = nullableStr(invID)
		it.BankEntityID = nullableStr(bankID)
		it.GLEntityID = nullableStr(glID)
		it.CreatedAt = createdAt
		out = append(out, it)
	}
	if out == nil {
		out = []item{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// HandleConfirm marks a reconciliation group as confirmed by the reviewer.
// RLS scopes the UPDATE to books the caller is assigned to; a cross-tenant or
// unassigned link matches zero rows and is reported as 404 (no existence leak).
func (s *Service) HandleConfirm(w http.ResponseWriter, r *http.Request) {
	s.updateLinkStatus(w, r, "confirmed", "confirm_link")
}

// HandleReject marks a reconciliation group as rejected by the reviewer.
func (s *Service) HandleReject(w http.ResponseWriter, r *http.Request) {
	s.updateLinkStatus(w, r, "rejected", "reject_link")
}

func (s *Service) updateLinkStatus(w http.ResponseWriter, r *http.Request, status, action string) {
	linkID := r.PathValue("linkId")
	if linkID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "linkId required")
		return
	}

	conn := middleware.GetConn(r.Context())
	if conn == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// Fetch the owning book first (RLS-scoped) so the audit log is accurate and
	// unassigned links resolve to 404 instead of a silent no-op.
	var clientBookID string
	err := conn.QueryRow(r.Context(),
		"SELECT client_book_id::text FROM reconciliation_groups WHERE id = $1", linkID).Scan(&clientBookID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "link not found")
			return
		}
		slog.Error("failed to load reconciliation group", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	_, err = conn.Exec(r.Context(),
		"UPDATE reconciliation_groups SET status = $1 WHERE id = $2", status, linkID)
	if err != nil {
		slog.Error("failed to update link status", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), clientBookID, action, linkID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "link " + status})
}

func (s *Service) HandleBulkConfirm(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	var req struct {
		LinkIDs []string `json:"link_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	if len(req.LinkIDs) == 0 {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "link_ids required")
		return
	}
	for _, id := range req.LinkIDs {
		_, err := middleware.DB(r.Context(), s.db).Exec(r.Context(),
			"UPDATE reconciliation_groups SET status = 'confirmed' WHERE id = $1 AND client_book_id = $2",
			id, bookID)
		if err != nil {
			slog.Error("failed to bulk confirm link", "error", err)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "links confirmed"})
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func decodeJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}
