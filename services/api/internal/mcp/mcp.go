package mcp

// Internal MCP tool server — called by services/agent-runtime, not external clients.
// Tools: get_pending_entities, create_entity_link, flag_for_review,
// get_book_tolerance.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

// VerificationPublisher publishes verification.requested after a group is
// linked, so the verify worker (pipeline.VerifyWorker) evaluates it and writes
// a finding. Injected by main.go; nil-safe (MCP works without NATS).
type VerificationPublisher interface {
	PublishVerification(ctx context.Context, groupID, clientBookID string) error
}

// Service holds no pool. Every statement in this file runs on the RLS-primed
// request connection, and the only thing the removed `db *pgxpool.Pool` field
// ever did was feed an Acquire() fallback in four handlers — so once those fail
// closed, the field was write-only. Fourth instance of that same shape after
// settings.go, push.go and the tenant package.
type Service struct {
	verifyPub VerificationPublisher
}

func NewService() *Service { return &Service{} }

func (s *Service) SetVerificationPublisher(p VerificationPublisher) { s.verifyPub = p }

// primed returns the RLS-primed request connection, or false after logging and
// writing a 500. ONE resolution point for this file (rule 13); before
// 2026-09-05 all four handlers carried a byte-identical copy of
// `c := GetConn(...); if c == nil { c2, _ := s.db.Acquire(...); c = c2 }`.
//
// Why there is no Acquire() fallback. mcpSvc.SetDB(pool) at main.go:241 passed
// the RLS-ENFORCED app pool, so Acquire() returned a connection with no
// app.current_firm set. Every table these handlers touch — extracted_entities,
// reconciliation_groups, reconciliation_group_members, client_books — has
// ENABLE ROW LEVEL SECURITY, and init.sql's policies cast current_setting(...)
// straight to uuid with no missing_ok, so such a statement raises (42704, or
// 22P02 on ''::uuid after ReleaseRLSConn RESETs it). The fallback could never
// have produced a working query; it only moved the failure to a place that
// could not explain it.
//
// OBSERVED: unreachable today, on a path worth spelling out because it is NOT
// the RLSInjector group. The four /mcp/tools/* routes (main.go:519-522) sit in a
// group whose only middleware is InternalAuth(pool, sysPool) (main.go:511).
// InternalAuth resolves book -> firm on sysDB, then calls
// middleware.AcquireScoped (internal_auth.go:110), which primes a connection
// from the app pool and stores it under connKey (middleware.go:200) — the same
// key GetConn reads (middleware.go:262). So GetConn is non-nil here for every
// authenticated MCP call. REASONED, from those four line references; not
// executed.
func (s *Service) primed(w http.ResponseWriter, r *http.Request) (middleware.Querier, bool) {
	if c := middleware.GetConn(r.Context()); c != nil {
		return c, true
	}
	slog.Error("mcp: no RLS-primed connection; the route is mounted outside both "+
		"RLSInjector and InternalAuth, and every statement here would raise",
		"path", r.URL.Path, "method", r.Method)
	writeProblem(w, http.StatusInternalServerError,
		"https://ai-auditor.dev/errors/internal", "no db conn")
	return nil, false
}


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
// agent-runtime can run extraction/classification on them. When batch_id is
// provided (a source document id), only that document's entities are returned —
// extraction is per-document, not per-book (a book can hold hundreds of
// entities; a single LLM prompt must not span the whole book).
func (s *Service) HandleGetPendingEntities(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientBookID string `json:"client_book_id"`
		BatchID      string `json:"batch_id"` // optional: scope to one source document
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientBookID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "client_book_id required")
		return
	}

	// Scoped by the RLS session vars InternalAuth primed on this connection —
	// not by RLSInjector, which this route group does not use. See s.primed.
	c, ok := s.primed(w, r)
	if !ok {
		return
	}

	// Batch scoping: when a batch_id (source document) is given, restrict to its
	// entities so the LLM prompt stays per-document, not per-book.
	query := `SELECT id::text, client_book_id::text, source_document_id::text, entity_type,
			COALESCE(entity_subtype, ''), amount_cents, currency, COALESCE(transaction_date, 'epoch'),
			COALESCE(counterparty, ''), COALESCE(description, ''), COALESCE(gl_account_code, ''),
			page_number, bbox, extraction_confidence, source_format
		 FROM extracted_entities
		 WHERE client_book_id = $1
		   AND id NOT IN (SELECT extracted_entity_id FROM reconciliation_group_members)`
	args := []interface{}{req.ClientBookID}
	if req.BatchID != "" {
		query += " AND source_document_id = $2"
		args = append(args, req.BatchID)
	}
	query += " ORDER BY extracted_at DESC LIMIT 500"
	rows, err := c.Query(r.Context(), query, args...)
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
		var txnDate time.Time
		var bboxJSON []byte
		if err := rows.Scan(&e.ID, &e.ClientBookID, &e.SourceDocumentID, &e.EntityType,
			&e.EntitySubtype, &e.AmountCents, &e.Currency, &txnDate, &e.Counterparty,
			&e.Description, &e.GLAccountCode, &e.PageNumber, &bboxJSON,
			&e.ExtractionConfidence, &e.SourceFormat); err != nil {
			slog.Warn("mcp pending scan fail", "error", err)
			continue
		}
		if !txnDate.IsZero() {
			e.TransactionDate = txnDate.Format("2006-01-02")
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
	// Groups need not have all three legs (bank+GL only is valid — deposits,
	// fees). At least bank+GL required; invoice may be empty.
	if len(req.BankIDs) == 0 || len(req.GLIDs) == 0 {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"bank_ids and gl_ids required (invoice_ids optional)")
		return
	}
	if req.Status == "" {
		req.Status = "needs_review"
	}
	// A caller may only propose a MACHINE disposition. The two human ones
	// ('confirmed', 'rejected') and 'superseded' are decisions this endpoint has
	// no standing to record, and accepting them here reopened the exact hole that
	// verify_worker.go's downgrade closes: that downgrade is guarded by
	// `AND status = 'auto_linked'` so it never overwrites a human decision, so a
	// group created as 'confirmed' is permanently immune to it. It would carry an
	// open exceeds_tolerance finding, read as human-confirmed, and never appear in
	// the review queue (review.go selects on status) — with no human involved at
	// any point. This endpoint is called by the agent runtime
	// (agent-runtime/mcp_client.py persist_groups), which only ever proposes
	// 'auto_linked' or 'needs_review'.
	//
	// Rejecting instead of silently coercing: a caller asking for a status it
	// cannot set is a wiring bug in the caller, and quietly turning it into
	// 'needs_review' would hide that while still throwing away the intent.
	// Unrecognized values previously reached the INSERT and failed the CHECK
	// constraint on reconciliation_groups.status as a bare 500 "insert failed".
	if req.Status != "auto_linked" && req.Status != "needs_review" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"status must be 'auto_linked' or 'needs_review'; human dispositions are "+
				"recorded through the review endpoints, not at link creation")
		return
	}

	c, ok := s.primed(w, r)
	if !ok {
		return
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
	// AR-side activity is categorized, not excluded.
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

	c, ok := s.primed(w, r)
	if !ok {
		return
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

	c, ok := s.primed(w, r)
	if !ok {
		return
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
