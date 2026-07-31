package review

import (
	"encoding/json"
	"log/slog"
	"net/http"

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

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": []interface{}{}, "next_cursor": nil})
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
		_, err := s.db.Exec(r.Context(),
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
