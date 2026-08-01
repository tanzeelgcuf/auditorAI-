package humanoverride

// Doc 11 (Round 5) — human override capability. When the automation gets it
// wrong, a reviewer can: create an entity manually, split/merge a group, and
// tag entities. Config mutations get audited. All history preserved (no deletes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ---- §1 Manual entity creation ----

// HandleCreateManualEntity lets a reviewer create an entity by hand when OCR/parsing
// failed or the transaction has no source document. Supersedes the original if
// corrects_entity_id is set (original stays as historical record).
func (s *Service) HandleCreateManualEntity(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	userID := middleware.GetUserID(r.Context())

	var req struct {
		EntityType       string  `json:"entity_type"` // invoice_line_item | bank_transaction | gl_entry
		AmountCents      int64   `json:"amount_cents"`
		TransactionDate  *string `json:"transaction_date"`
		Counterparty     *string `json:"counterparty"`
		Description      *string `json:"description"`
		GLAccountCode    *string `json:"gl_account_code"`
		SourceDocumentID *string `json:"source_document_id"`
		CorrectsEntityID *string `json:"corrects_entity_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	if req.EntityType != "invoice_line_item" && req.EntityType != "bank_transaction" && req.EntityType != "gl_entry" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid entity_type")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	tx, err := c.Begin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context())

	var newID string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO extracted_entities
			(client_book_id, source_document_id, entity_type, amount_cents, transaction_date,
			 counterparty, description, gl_account_code, page_number, bbox, extraction_confidence,
			 source_format, created_by, manually_created_by, corrects_entity_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, '{}', 1.0, 'manual', 'manual', $9, $10)
		 RETURNING id::text`,
		bookID, req.SourceDocumentID, req.EntityType, req.AmountCents, req.TransactionDate,
		req.Counterparty, req.Description, req.GLAccountCode, userID, req.CorrectsEntityID).Scan(&newID)
	if err != nil {
		slog.Error("failed to create manual entity", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}

	// Supersede the original if a correction target was given.
	if req.CorrectsEntityID != nil && *req.CorrectsEntityID != "" {
		_, err = tx.Exec(r.Context(),
			`UPDATE extracted_entities SET status = 'superseded_by_manual' WHERE id = $1`,
			*req.CorrectsEntityID)
		if err != nil {
			slog.Error("failed to supersede entity", "error", err)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "supersede failed")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "commit failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "create_manual_entity", newID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": newID, "client_book_id": bookID, "entity_type": req.EntityType,
		"amount_cents": req.AmountCents, "source_format": "manual",
	})
}

// ---- §2 Group split / merge ----

// HandleSplitGroup repartitions a group's members into new groups. Old group
// marked superseded (history preserved).
func (s *Service) HandleSplitGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("groupId")
	if groupID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "groupId required")
		return
	}
	userID := middleware.GetUserID(r.Context())

	var req struct {
		Partitions [][]string `json:"partitions"` // each partition = entity IDs that form a new group
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Partitions) < 2 {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"partitions (at least 2 arrays of entity ids) required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	tx, err := c.Begin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context())

	// Verify the source group exists and load its book.
	var bookID string
	err = tx.QueryRow(r.Context(),
		"SELECT client_book_id::text FROM reconciliation_groups WHERE id = $1", groupID).Scan(&bookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "group not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	// Mark the old group superseded.
	_, err = tx.Exec(r.Context(),
		"UPDATE reconciliation_groups SET status = 'superseded' WHERE id = $1", groupID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "supersede failed")
		return
	}

	var newGroupIDs []string
	for _, partition := range req.Partitions {
		if len(partition) == 0 {
			continue
		}
		var newID string
		err = tx.QueryRow(r.Context(),
			`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status)
			 VALUES ($1, 1.0, 'needs_review') RETURNING id::text`, bookID).Scan(&newID)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
			return
		}
		for _, entityID := range partition {
			_, err = tx.Exec(r.Context(),
				`INSERT INTO reconciliation_group_members (reconciliation_group_id, extracted_entity_id, role)
				 SELECT $1, id, role FROM reconciliation_group_members WHERE extracted_entity_id = $2 AND reconciliation_group_id = $3
				 ON CONFLICT DO NOTHING`,
				newID, entityID, groupID)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "member insert failed")
				return
			}
		}
		newGroupIDs = append(newGroupIDs, newID)
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "commit failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "split_group", groupID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"superseded_group_id": groupID, "new_group_ids": newGroupIDs,
	})
}

// HandleMergeGroups combines multiple groups into one. Source groups superseded.
func (s *Service) HandleMergeGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupIDs []string `json:"group_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.GroupIDs) < 2 {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"group_ids (at least 2) required")
		return
	}
	userID := middleware.GetUserID(r.Context())

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	tx, err := c.Begin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context())

	var bookID string
	err = tx.QueryRow(r.Context(),
		"SELECT client_book_id::text FROM reconciliation_groups WHERE id = $1", req.GroupIDs[0]).Scan(&bookID)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "group not found")
		return
	}

	var newID string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status)
		 VALUES ($1, 1.0, 'needs_review') RETURNING id::text`, bookID).Scan(&newID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}

	for _, gid := range req.GroupIDs {
		_, err = tx.Exec(r.Context(),
			`UPDATE reconciliation_groups SET status = 'superseded' WHERE id = $1`, gid)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "supersede failed")
			return
		}
		_, err = tx.Exec(r.Context(),
			`INSERT INTO reconciliation_group_members (reconciliation_group_id, extracted_entity_id, role)
			 SELECT $1, extracted_entity_id, role FROM reconciliation_group_members WHERE reconciliation_group_id = $2
			 ON CONFLICT DO NOTHING`, newID, gid)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "member insert failed")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "commit failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "merge_groups", newID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"new_group_id": newID, "superseded_group_ids": req.GroupIDs,
	})
}

// ---- §3 Config change audit ----

// LogConfigChange records a settings mutation. Called by settings/tenant handlers.
func LogConfigChange(ctx context.Context, db *pgxpool.Pool, bookID, userID, field string, oldV, newV interface{}) {
	if db == nil || bookID == "" {
		return
	}
	_, err := db.Exec(ctx,
		`INSERT INTO config_change_log (client_book_id, changed_by, field_name, old_value, new_value)
		 VALUES ($1, $2, $3, $4, $5)`,
		bookID, userID, field, strVal(oldV), strVal(newV))
	if err != nil {
		slog.Warn("failed to log config change", "error", err)
	}
}

func strVal(v interface{}) *string {
	if v == nil {
		return nil
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case int:
		s = itoa(t)
	case int64:
		s = itoa64(t)
	case float64:
		s = fmtFloat(t)
	case bool:
		if t {
			s = "true"
		} else {
			s = "false"
		}
	default:
		s = fmt.Sprint(v)
	}
	return &s
}

func fmtFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// HandleConfigHistory lists config changes for a book.
func (s *Service) HandleConfigHistory(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT field_name, COALESCE(old_value,''), COALESCE(new_value,''), changed_by::text, changed_at
		 FROM config_change_log WHERE client_book_id = $1 ORDER BY changed_at DESC LIMIT 100`, bookID)
	if err != nil {
		slog.Error("config history query failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type entry struct {
		Field     string    `json:"field_name"`
		OldValue  string    `json:"old_value"`
		NewValue  string    `json:"new_value"`
		ChangedBy string    `json:"changed_by"`
		ChangedAt time.Time `json:"changed_at"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Field, &e.OldValue, &e.NewValue, &e.ChangedBy, &e.ChangedAt); err != nil {
			continue
		}
		out = append(out, e)
	}
	if out == nil {
		out = []entry{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

// ---- §6 Tags ----

// HandleCreateTag creates a firm-level tag.
func (s *Service) HandleCreateTag(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized", "unauthorized")
		return
	}
	var req struct {
		Label string  `json:"label"`
		Color *string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "label required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var tagID string
	err := c.QueryRow(r.Context(),
		`INSERT INTO tags (firm_id, label, color) VALUES ($1, $2, $3)
		 ON CONFLICT (firm_id, label) DO UPDATE SET color = EXCLUDED.color RETURNING id::text`,
		firmID, req.Label, req.Color).Scan(&tagID)
	if err != nil {
		slog.Error("failed to create tag", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": tagID, "label": req.Label})
}

// HandleListTags lists firm-level tags.
func (s *Service) HandleListTags(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized", "unauthorized")
		return
	}
	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}
	rows, err := c.Query(r.Context(),
		"SELECT id::text, label, COALESCE(color,'') FROM tags WHERE firm_id = $1 ORDER BY label", firmID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()
	type tag struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Color string `json:"color"`
	}
	var out []tag
	for rows.Next() {
		var t tag
		if err := rows.Scan(&t.ID, &t.Label, &t.Color); err != nil {
			continue
		}
		out = append(out, t)
	}
	if out == nil {
		out = []tag{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

// HandleTagEntity attaches a tag to an entity (must be in an assigned book).
func (s *Service) HandleTagEntity(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r.Context())
	var req struct {
		EntityID string `json:"entity_id"`
		TagID    string `json:"tag_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EntityID == "" || req.TagID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "entity_id and tag_id required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// RLS scopes the entity lookup; a foreign entity returns no row -> 404.
	var bookID string
	err := c.QueryRow(r.Context(),
		"SELECT client_book_id::text FROM extracted_entities WHERE id = $1", req.EntityID).Scan(&bookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "entity not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	_, err = c.Exec(r.Context(),
		`INSERT INTO entity_tags (extracted_entity_id, tag_id, tagged_by) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`, req.EntityID, req.TagID, userID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "entity tagged"})
}

// ---- §5 Automation rate ----

// HandleAutomationRate returns the automation-rate view for a book.
func (s *Service) HandleAutomationRate(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}
	var auto, review, confirmed, total int
	err := c.QueryRow(r.Context(),
		`SELECT auto_linked_count, needs_review_count, confirmed_count, total_count
		 FROM book_automation_rate WHERE client_book_id = $1`, bookID).
		Scan(&auto, &review, &confirmed, &total)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"auto_linked": 0, "needs_review": 0, "confirmed": 0, "total": 0, "automation_rate": 0,
			})
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	rate := 0.0
	if total > 0 {
		rate = float64(auto) / float64(total)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"auto_linked": auto, "needs_review": review, "confirmed": confirmed,
		"total": total, "automation_rate": rate,
	})
}
