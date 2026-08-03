package mcp

// Internal MCP tool server — called by services/agent-runtime, not external clients.
// Tools (docs 05 §3): get_pending_entities, create_entity_link, flag_for_review,
// get_book_tolerance.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

// VerificationPublisher publishes verification.requested after a group is
// linked, so the verify worker (pipeline.VerifyWorker) evaluates it and writes
// a finding. Injected by main.go; nil-safe (MCP works without NATS).
type VerificationPublisher interface {
	PublishVerification(ctx context.Context, groupID, clientBookID string) error
}

type Service struct {
	db            *pgxpool.Pool
	verifyPub     VerificationPublisher
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

func (s *Service) SetVerificationPublisher(p VerificationPublisher) { s.verifyPub = p }

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

// HandleGetPendingEntities returns unclassified/unlinked entities for a book so
// agent-runtime can run extraction/classification on them.
func (s *Service) HandleGetPendingEntities(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientBookID string `json:"client_book_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientBookID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "client_book_id required")
		return
	}

	// Scoped by RLS session vars set by the middleware chain; agent-runtime calls
	// come through the same authenticated path as the web app.
	c := middleware.GetConn(r.Context())
	if c == nil {
		c2, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
			return
		}
		defer c2.Release()
		c = c2
	}

	rows, err := c.Query(r.Context(),
		`SELECT id::text, client_book_id::text, source_document_id::text, entity_type,
			entity_subtype, amount_cents, currency, COALESCE(transaction_date, 'epoch'),
			COALESCE(counterparty, ''), COALESCE(description, ''), COALESCE(gl_account_code, ''),
			page_number, bbox, extraction_confidence, source_format
		 FROM extracted_entities
		 WHERE client_book_id = $1
		   AND id NOT IN (SELECT extracted_entity_id FROM reconciliation_group_members)
		 ORDER BY extracted_at DESC LIMIT 500`, req.ClientBookID)
	if err != nil {
		slog.Error("failed to query pending entities", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type entity struct {
		ID                  string    `json:"id"`
		ClientBookID        string    `json:"client_book_id"`
		SourceDocumentID    string    `json:"source_document_id"`
		EntityType          string    `json:"entity_type"`
		EntitySubtype       string    `json:"entity_subtype"`
		AmountCents         int64     `json:"amount_cents"`
		Currency            string    `json:"currency"`
		TransactionDate     string    `json:"transaction_date"`
		Counterparty        string    `json:"counterparty"`
		Description         string    `json:"description"`
		GLAccountCode       string    `json:"gl_account_code"`
		PageNumber          int       `json:"page_number"`
		BBox                map[string]float64 `json:"bbox"`
		ExtractionConfidence float64  `json:"extraction_confidence"`
		SourceFormat        string    `json:"source_format"`
	}

	var out []entity
	for rows.Next() {
		var e entity
		var txnDate string
		var bboxJSON []byte
		if err := rows.Scan(&e.ID, &e.ClientBookID, &e.SourceDocumentID, &e.EntityType,
			&e.EntitySubtype, &e.AmountCents, &e.Currency, &txnDate, &e.Counterparty,
			&e.Description, &e.GLAccountCode, &e.PageNumber, &bboxJSON,
			&e.ExtractionConfidence, &e.SourceFormat); err != nil {
			continue
		}
		if txnDate != "0001-01-01T00:00:00Z" {
			e.TransactionDate = txnDate[:10]
		}
		e.BBox = map[string]float64{}
		if len(bboxJSON) > 0 {
			_ = json.Unmarshal(bboxJSON, &e.BBox)
		}
		out = append(out, e)
	}
	if out == nil {
		out = []entity{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"entities": out})
}

// HandleCreateEntityLink creates a reconciliation group from agent-runtime's
// cross-linking output.
func (s *Service) HandleCreateEntityLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InvoiceIDs []string `json:"invoice_ids"`
		BankIDs    []string `json:"bank_ids"`
		GLIDs      []string `json:"gl_ids"`
		Confidence float64  `json:"confidence"`
		Status     string   `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	// Doc 09: groups need not have all three legs (bank+GL only is valid — deposits,
	// fees). At least bank+GL required; invoice may be empty.
	if len(req.BankIDs) == 0 || len(req.GLIDs) == 0 {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"bank_ids and gl_ids required (invoice_ids optional)")
		return
	}
	if req.Status == "" {
		req.Status = "needs_review"
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		c2, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
			return
		}
		defer c2.Release()
		c = c2
	}

	tx, err := c.Begin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context())

	// Determine the book from the first entity present (invoice, else bank, else GL)
	// — all must be same book (verified by RLS).
	firstID := ""
	if len(req.InvoiceIDs) > 0 {
		firstID = req.InvoiceIDs[0]
	} else if len(req.BankIDs) > 0 {
		firstID = req.BankIDs[0]
	} else {
		firstID = req.GLIDs[0]
	}
	var bookID string
	err = tx.QueryRow(r.Context(),
		"SELECT client_book_id::text FROM extracted_entities WHERE id = $1",
		firstID).Scan(&bookID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "entity not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	// Derive group scope from the GL legs' chart-of-accounts account type
	// (doc 12 §2 / Round 7): AR-side activity is categorized, not excluded.
	// A GL leg posting to AR / asset / revenue accounts => 'ar'; else 'ap'.
	var scope string
	err = tx.QueryRow(r.Context(),
		`SELECT CASE WHEN count(*) > 0 THEN 'ar' ELSE 'ap' END
		 FROM extracted_entities e
		 LEFT JOIN chart_of_accounts coa
		   ON coa.client_book_id = e.client_book_id
		  AND (coa.account_code = e.gl_account_code OR coa.account_name = e.gl_account_code)
		 WHERE e.id = ANY($1)
		   AND (coa.account_type IN ('asset','revenue')
		        OR e.gl_account_code ILIKE '%checking%' OR e.gl_account_code ILIKE '%cash%'
		        OR e.gl_account_code ILIKE '%receivable%')`,
		req.GLIDs).Scan(&scope)
	if err != nil {
		scope = "ap"
	}

	var groupID string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status, group_scope)
		 VALUES ($1, $2, $3, $4) RETURNING id::text`,
		bookID, req.Confidence, req.Status, scope).Scan(&groupID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}

	insertMember := func(entityID, role string) error {
		_, err := tx.Exec(r.Context(),
			`INSERT INTO reconciliation_group_members (reconciliation_group_id, extracted_entity_id, role)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, groupID, entityID, role)
		return err
	}
	for _, id := range req.InvoiceIDs {
		if err := insertMember(id, "invoice"); err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "member insert failed")
			return
		}
	}
	for _, id := range req.BankIDs {
		if err := insertMember(id, "bank"); err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "member insert failed")
			return
		}
	}
	for _, id := range req.GLIDs {
		if err := insertMember(id, "gl"); err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "member insert failed")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "commit failed")
		return
	}

	// Fire-and-forget: ask the verify worker to evaluate this group and write
	// a finding (Prompt 3 wiring — verification.requested had no consumer).
	if s.verifyPub != nil {
		if err := s.verifyPub.PublishVerification(r.Context(), groupID, bookID); err != nil {
			slog.Error("verification publish failed", "group", groupID, "error", err)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": groupID, "client_book_id": bookID, "status": req.Status,
		"link_confidence": req.Confidence,
	})
}

// HandleFlagForReview marks a low-confidence link for human review.
func (s *Service) HandleFlagForReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EntityLinkID string `json:"entity_link_id"`
		Reason       string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EntityLinkID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "entity_link_id required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		c2, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
			return
		}
		defer c2.Release()
		c = c2
	}

	_, err := c.Exec(r.Context(),
		"UPDATE reconciliation_groups SET status = 'needs_review' WHERE id = $1", req.EntityLinkID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "flagged for review"})
}

// HandleGetBookTolerance returns the book's reconciliation config for agent-runtime.
func (s *Service) HandleGetBookTolerance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientBookID string `json:"client_book_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientBookID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "client_book_id required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		c2, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
			return
		}
		defer c2.Release()
		c = c2
	}

	var tolerance, toleranceMode string
	var autoLink, reviewFloor float64
	err := c.QueryRow(r.Context(),
		`SELECT reconciliation_tolerance_cents, tolerance_mode,
			COALESCE(auto_link_confidence_threshold, 0.85), COALESCE(review_confidence_floor, 0.50)
		 FROM client_books WHERE id = $1`, req.ClientBookID).
		Scan(&tolerance, &toleranceMode, &autoLink, &reviewFloor)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tolerance_cents":        tolerance,
		"tolerance_mode":         toleranceMode,
		"auto_link_threshold":    autoLink,
		"review_floor":           reviewFloor,
	})
}
