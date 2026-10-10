// Package auditlog reads the access audit trail back to the product and
// sweeps it for retention. Until 2026-10-01 access_log was write-only from
// the product's perspective: the rows were stored and surfaced to nobody,
// which for a product whose value proposition is traceability was a hole in
// the demo before it was a hole in the compliance matrix
// (SOC2_READINESS.md roadmap item 10).
package auditlog

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

// SetDB hands the service the app pool. The handler reads through
// middleware.GetConn (the request's RLS-primed connection) per rule 14; the
// pool is what GetConn falls back to for pre-primed surfaces.
func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

// HandleBookAccessLog serves the book's audit trail: who did what, when, and
// from where. Book-scoped, behind the assigned-books gate and the
// RLS-primed request connection — the same as every other /v1/books route.
//
// source_ip reads through host(), NOT ::text: on this stack (postgres:16)
// ::text renders a stored bare address as 203.0.113.9/32, and the audit trail
// must agree with what the rate limiter bucketed (rule 13). NULL source_ip
// reads as "" — an entry admitting it does not know, the same posture as the
// write side.
//
// The cursor is the last id (BIGSERIAL): the next page is ?before=<id>, and
// rows are ordered by occurred_at DESC, id DESC — a stable tiebreak, so a
// page boundary never skips or repeats a row that shares a timestamp.
func (s *Service) HandleBookAccessLog(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	assigned := middleware.GetAssignedBooks(r.Context())
	found := false
	for _, b := range assigned {
		if b == bookID {
			found = true
			break
		}
	}
	if bookID == "" || !found {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	query := `SELECT id, user_id::text,
			COALESCE(client_book_id::text, ''),
			action, COALESCE(resource_id::text, ''),
			COALESCE(host(source_ip), ''),
			occurred_at
		  FROM access_log WHERE client_book_id = $1`
	args := []interface{}{bookID}
	if before := r.URL.Query().Get("before"); before != "" {
		if id, err := strconv.ParseInt(before, 10, 64); err == nil {
			query += " AND id < $2"
			args = append(args, id)
		}
	}
	query += " ORDER BY occurred_at DESC, id DESC LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit)

	rows, err := c.Query(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to read access log", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type entry struct {
		ID           int64     `json:"id"`
		UserID       string    `json:"user_id"`
		ClientBookID string    `json:"client_book_id"`
		Action       string    `json:"action"`
		ResourceID   string    `json:"resource_id"`
		SourceIP     string    `json:"source_ip"`
		OccurredAt   time.Time `json:"occurred_at"`
	}
	out := make([]entry, 0, limit)
	var lastID int64
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.UserID, &e.ClientBookID, &e.Action,
			&e.ResourceID, &e.SourceIP, &e.OccurredAt); err != nil {
			continue
		}
		out = append(out, e)
		lastID = e.ID
	}
	nextCursor := interface{}(nil)
	if len(out) == limit && lastID > 0 {
		nextCursor = lastID
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nextCursor})
}
