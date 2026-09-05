package findings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/storage"
)

type Service struct {
	db *pgxpool.Pool
	// storage verifies that an attachment's storage_key points at an object that
	// actually exists. Attachment bytes are PUT directly to storage by the client
	// via the presigned flow, so the API never holds them — existence
	// verification, not writing, is what this service needs it for. May be nil
	// when storage.New() failed at startup; HandleAddAttachment answers 503.
	storage *storage.Client
	// Notifier delivers report.generated webhook events (doc 07 §7). Injected by
	// main.go to avoid an import cycle (webhooks imports nothing from findings).
	Notifier ReportNotifier
}

// ReportNotifier is satisfied by *webhooks.Service.
type ReportNotifier interface {
	NotifyReportGenerated(ctx context.Context, firmID, reportID string) error
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

func (s *Service) SetStorage(st *storage.Client) { s.storage = st }

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

// ---- findings CRUD ----

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	severity := r.URL.Query().Get("severity")
	status := r.URL.Query().Get("status")
	limit := 25
	query := `SELECT id::text, client_book_id::text, reconciliation_group_id::text, rule_id,
		rule_version, calculated_variance_cents, tolerance_cents, exceeds_tolerance,
		calculation_formula, severity, status, prepared_by::text, reviewed_by::text,
		COALESCE(reviewed_at, 'epoch'), created_at
		FROM audit_findings WHERE client_book_id = $1`
	args := []interface{}{bookID}
	if severity != "" {
		args = append(args, severity)
		query += fmt.Sprintf(" AND severity = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT %d", limit)

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}
	rows, err := c.Query(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to list findings", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type findingRow struct {
		ID                     string    `json:"id"`
		ClientBookID           string    `json:"client_book_id"`
		ReconciliationGroupID  string    `json:"reconciliation_group_id"`
		RuleID                 string    `json:"rule_id"`
		RuleVersion            string    `json:"rule_version"`
		CalculatedVarianceCents int64    `json:"calculated_variance_cents"`
		ToleranceCents         int64     `json:"tolerance_cents"`
		ExceedsTolerance       bool      `json:"exceeds_tolerance"`
		CalculationFormula     string    `json:"calculation_formula"`
		Severity               string    `json:"severity"`
		Status                 string    `json:"status"`
		PreparedBy             *string   `json:"prepared_by"`
		ReviewedBy             *string   `json:"reviewed_by"`
		ReviewedAt             time.Time `json:"reviewed_at"`
		CreatedAt              time.Time `json:"created_at"`
	}

	var out []findingRow
	for rows.Next() {
		var f findingRow
		var reviewedAt time.Time
		if err := rows.Scan(&f.ID, &f.ClientBookID, &f.ReconciliationGroupID, &f.RuleID,
			&f.RuleVersion, &f.CalculatedVarianceCents, &f.ToleranceCents, &f.ExceedsTolerance,
			&f.CalculationFormula, &f.Severity, &f.Status, &f.PreparedBy, &f.ReviewedBy,
			&reviewedAt, &f.CreatedAt); err != nil {
			continue
		}
		if !reviewedAt.IsZero() {
			f.ReviewedAt = reviewedAt
		}
		out = append(out, f)
	}
	if out == nil {
		out = []findingRow{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

func (s *Service) HandleAddComment(w http.ResponseWriter, r *http.Request) {
	findingID := r.PathValue("findingId")
	if findingID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "findingId required")
		return
	}
	userID := middleware.GetUserID(r.Context())
	var req struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Comment) == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "comment is required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}
	_, err := c.Exec(r.Context(),
		`INSERT INTO finding_comments (audit_finding_id, user_id, comment) VALUES ($1, $2, $3)`,
		findingID, userID, req.Comment)
	if err != nil {
		slog.Error("failed to add comment", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "comment added"})
}

func (s *Service) HandleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	findingID := r.PathValue("findingId")
	if findingID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "findingId required")
		return
	}
	userID := middleware.GetUserID(r.Context())
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	if req.Status != "open" && req.Status != "acknowledged" && req.Status != "resolved" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid status")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// If resolving, set reviewed_by/reviewed_at unless already reviewed (doc 10 §3)
	if req.Status == "resolved" {
		var reviewedBy *string
		_ = c.QueryRow(r.Context(),
			"SELECT reviewed_by FROM audit_findings WHERE id = $1", findingID).Scan(&reviewedBy)
		if reviewedBy == nil {
			_, err := c.Exec(r.Context(),
				`UPDATE audit_findings SET status = $1, reviewed_by = $2, reviewed_at = now() WHERE id = $3`,
				req.Status, userID, findingID)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
				return
			}
		} else {
			_, err := c.Exec(r.Context(),
				`UPDATE audit_findings SET status = $1 WHERE id = $2`, req.Status, findingID)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
				return
			}
		}
	} else {
		_, err := c.Exec(r.Context(),
			`UPDATE audit_findings SET status = $1 WHERE id = $2`, req.Status, findingID)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "update failed")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "status updated"})
}

func (s *Service) HandleAddAttachment(w http.ResponseWriter, r *http.Request) {
	findingID := r.PathValue("findingId")
	if findingID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "findingId required")
		return
	}
	userID := middleware.GetUserID(r.Context())
	var req struct {
		StorageKey string `json:"storage_key"`
		Filename   string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StorageKey == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "storage_key required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// storage_key ARRIVES FROM THE CLIENT AND MUST NOT BE TRUSTED VERBATIM.
	//
	// RLS on finding_attachments constrains audit_finding_id (the policy is
	// FOR ALL with only a USING clause, and Postgres reuses USING as WITH CHECK
	// when the latter is omitted, so the INSERT is checked — a caller cannot
	// attach to another firm's finding). It says NOTHING about storage_key: that
	// column is a free TEXT field pointing into a shared bucket.
	//
	// Without this check, a caller legitimately assigned to book A could post
	// storage_key "documents/<book-B-uuid>/<uuid>-payroll.pdf" and record another
	// firm's document as evidence on their own finding — RLS-legal, and invisible
	// to every row-level test. Object keys are namespaced by book
	// (documents/<bookId>/... — see documents.go storageKey), so the server can
	// and must verify the namespace itself rather than accepting the client's.
	//
	// The finding's book is read through the RLS-scoped connection, so a finding
	// outside the caller's assignment returns no rows -> 404, with no existence
	// leak.
	var bookID string
	if err := c.QueryRow(r.Context(),
		`SELECT client_book_id::text FROM audit_findings WHERE id = $1`,
		findingID).Scan(&bookID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "finding not found")
			return
		}
		slog.Error("failed to resolve finding book", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	// Exact-prefix match, plus an explicit traversal reject: the prefix alone
	// would still admit "documents/<book>/../<other-book>/x.pdf", which resolves
	// outside the namespace in any client that normalises the path.
	wantPrefix := "documents/" + bookID + "/"
	if !strings.HasPrefix(req.StorageKey, wantPrefix) ||
		strings.Contains(req.StorageKey, "..") ||
		strings.Contains(req.StorageKey, "\\") {
		slog.Warn("rejected attachment with out-of-namespace storage key",
			"finding_id", findingID, "book_id", bookID, "user_id", userID)
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/invalid-storage-key",
			"storage_key must reference an object uploaded to this finding's client book")
		return
	}

	// The key must also point at an object that EXISTS. Namespace validation
	// above stops a caller naming another firm's object; this stops an attachment
	// row that references nothing at all — the same orphan-key defect that
	// documents.HandleUpload shipped, arriving by a different route. The API never
	// holds these bytes (the client PUTs them directly via the presigned flow), so
	// existence is verified rather than written.
	if s.storage == nil {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"storage not configured")
		return
	}
	exists, err := s.storage.ObjectExists(r.Context(), req.StorageKey)
	if err != nil {
		slog.Error("storage unreachable during attachment add", "error", err, "finding_id", findingID)
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/storage-unavailable",
			"could not verify the attachment because storage is unreachable — retry shortly")
		return
	}
	if !exists {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/upload-incomplete",
			"no object exists at that storage_key — upload the file before attaching it")
		return
	}

	if _, err := c.Exec(r.Context(),
		`INSERT INTO finding_attachments (audit_finding_id, uploaded_by, storage_key, filename)
		 VALUES ($1, $2, $3, $4)`,
		findingID, userID, req.StorageKey, req.Filename); err != nil {
		slog.Error("failed to add attachment", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "attachment added"})
}

// ---- report generation + traceability ----

func (s *Service) HandleGenerateReport(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	userID := middleware.GetUserID(r.Context())

	var req struct {
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	start, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "period_start must be YYYY-MM-DD")
		return
	}
	end, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "period_end must be YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "period_end before period_start")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// Gather findings for the period. A finding belongs to the period if any of its
	// reconciliation group members' entities carry a transaction_date in range
	// (not created_at — that's ingestion time, not the business period).
	rows, err := c.Query(r.Context(),
		`SELECT DISTINCT f.id::text, f.rule_id, f.rule_version, f.calculated_variance_cents,
			f.tolerance_cents, f.exceeds_tolerance, f.calculation_formula, f.severity, f.status,
			f.created_at
		 FROM audit_findings f
		 JOIN reconciliation_group_members m ON m.reconciliation_group_id = f.reconciliation_group_id
		 JOIN extracted_entities e ON e.id = m.extracted_entity_id
		 WHERE f.client_book_id = $1
		   AND e.transaction_date >= $2 AND e.transaction_date < $3
		 ORDER BY f.created_at DESC`,
		bookID, start, end.Add(24*time.Hour))
	if err != nil {
		slog.Error("failed to query findings for report", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	findingIDs := []string{}
	var pdfData reportData
	for rows.Next() {
		var id, ruleID, ruleVer, formula, sev, st string
		var variance, tolerance int64
		var exceeds bool
		var createdAt time.Time
		if err := rows.Scan(&id, &ruleID, &ruleVer, &variance, &tolerance, &exceeds,
			&formula, &sev, &st, &createdAt); err != nil {
			continue
		}
		findingIDs = append(findingIDs, id)
		pdfData.findings = append(pdfData.findings, findingEntry{
			ID: id, RuleID: ruleID, RuleVersion: ruleVer, VarianceCents: variance,
			ToleranceCents: tolerance, Exceeds: exceeds, Formula: formula,
			Severity: sev, Status: st,
		})
	}

	var reportID string
	err = c.QueryRow(r.Context(),
		`INSERT INTO audit_reports (client_book_id, period_start, period_end, generated_by, finding_ids)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
		bookID, start, end, userID, findingIDs).Scan(&reportID)
	if err != nil {
		slog.Error("failed to create report", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}

	// Generate PDF via Typst (self-hosted per docs). The rendered PDF is written to
	// the local reports dir (S3 upload deferred — see go-live checklist).
	pdfKey := ""
	if pdfPath, err := renderTypstReport(&pdfData, reportID); err != nil {
		slog.Warn("typst render skipped", "error", err)
	} else {
		pdfKey = pdfPath
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "generate_report", reportID)
	if s.Notifier != nil {
		if err := s.Notifier.NotifyReportGenerated(r.Context(), middleware.GetFirmID(r.Context()), reportID); err != nil {
			slog.Warn("report.generated webhook notification failed", "error", err)
		}
	}
	body, _ := middleware.EncodeJSON(map[string]interface{}{
		"id": reportID, "client_book_id": bookID,
		"period_start": req.PeriodStart, "period_end": req.PeriodEnd,
		"generated_at": time.Now().Format(time.RFC3339),
		"finding_ids": findingIDs, "pdf_storage_key": pdfKey,
	})
	if err := middleware.StoreIdempotentResponse(r.Context(), s.db, http.StatusCreated, body); err != nil {
		// The response below is already decided; this only means a retry of this
		// request will regenerate the report instead of replaying it.
		slog.Error("idempotency store failed", "error", err, "report_id", reportID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
}

func (s *Service) HandleGetReport(w http.ResponseWriter, r *http.Request) {
	reportID := r.PathValue("reportId")
	if reportID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "reportId required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var id, bookID, periodStart, periodEnd, generatedAt string
	var findingIDs []string
	var pdfKey *string
	err := c.QueryRow(r.Context(),
		`SELECT id::text, client_book_id::text, to_char(period_start,'YYYY-MM-DD'),
			to_char(period_end,'YYYY-MM-DD'), to_char(generated_at,'YYYY-MM-DD"T"HH24:MI:SS'),
			finding_ids, pdf_storage_key
		 FROM audit_reports WHERE id = $1`, reportID).
		Scan(&id, &bookID, &periodStart, &periodEnd, &generatedAt, &findingIDs, &pdfKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "report not found")
			return
		}
		slog.Error("failed to get report", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), bookID, "download_report", reportID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "client_book_id": bookID,
		"period_start": periodStart, "period_end": periodEnd,
		"generated_at": generatedAt, "finding_ids": findingIDs, "pdf_storage_key": pdfKey,
	})
}

// HandleGetCitation returns the exact source region (document, page, bbox) that
// produced a finding — the product's core trust mechanism (doc 04/05).
func (s *Service) HandleGetCitation(w http.ResponseWriter, r *http.Request) {
	reportID := r.PathValue("reportId")
	findingID := r.PathValue("findingId")
	if reportID == "" || findingID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "reportId and findingId required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// Verify the finding belongs to this report (RLS already scopes to assigned books)
	var inReport bool
	err := c.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM audit_reports WHERE id = $1 AND $2 = ANY(finding_ids))`,
		reportID, findingID).Scan(&inReport)
	if err != nil || !inReport {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "finding not in report")
		return
	}

	// Pull the source document + bbox for the finding's reconciliation group members.
	// We surface the first supporting entity's citation (invoice role preferred).
	type bbox struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	type cit struct {
		SourceDocumentID string `json:"source_document_id"`
		PageNumber       int    `json:"page_number"`
		BBox             *bbox  `json:"bbox"`
		RuleID           string `json:"rule_id"`
		RuleVersion      string `json:"rule_version"`
	}

	row := c.QueryRow(r.Context(),
		`SELECT e.source_document_id::text, e.page_number, e.bbox,
			f.rule_id, f.rule_version, f.client_book_id::text
		 FROM audit_findings f
		 JOIN reconciliation_group_members m ON m.reconciliation_group_id = f.reconciliation_group_id
		 JOIN extracted_entities e ON e.id = m.extracted_entity_id
		 WHERE f.id = $1
		 ORDER BY CASE m.role WHEN 'invoice' THEN 0 WHEN 'bank' THEN 1 ELSE 2 END
		 LIMIT 1`, findingID)

	var sourceDoc string
	var page int
	var bboxJSON []byte
	var ruleID, ruleVer, clientBookID string
	if err := row.Scan(&sourceDoc, &page, &bboxJSON, &ruleID, &ruleVer, &clientBookID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "no citation for finding")
			return
		}
		slog.Error("failed to get citation", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	var b bbox
	if len(bboxJSON) > 0 {
		_ = json.Unmarshal(bboxJSON, &b)
	}

	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), clientBookID, "view_finding", findingID)
	writeJSON(w, http.StatusOK, cit{
		SourceDocumentID: sourceDoc, PageNumber: page, BBox: &b,
		RuleID: ruleID, RuleVersion: ruleVer,
	})
}

// ---- traceability guard: hash helper (rule_version provenance) ----

func ruleVersionHash(ruleContent string) string {
	h := sha256.Sum256([]byte(ruleContent))
	return hex.EncodeToString(h[:8])
}

// ---- Typst report source ----

type findingEntry struct {
	ID             string
	RuleID         string
	RuleVersion    string
	VarianceCents  int64
	ToleranceCents int64
	Exceeds        bool
	Formula        string
	Severity       string
	Status         string
}

type reportData struct {
	findings []findingEntry
}

func buildTypstSource(d *reportData) string {
	var b strings.Builder
	b.WriteString("#set page(\"a4\")\n")
	b.WriteString("#set text(size: 10pt)\n")
	b.WriteString("= Audit Report\n")
	b.WriteString("#v(1cm)\n")

	sevCount := map[string]int{}
	for _, f := range d.findings {
		sevCount[f.Severity]++
	}
	b.WriteString("#h(1fr) Findings by severity:\n\n")
	for _, sev := range []string{"info", "low", "medium", "high"} {
		fmt.Fprintf(&b, "- %s: %d\n", sev, sevCount[sev])
	}
	b.WriteString("\n== Findings\n\n")
	for _, f := range d.findings {
		fmt.Fprintf(&b, "*%s* (%s v%s) — severity: %s, status: %s\n\n",
			f.ID, f.RuleID, f.RuleVersion, f.Severity, f.Status)
		b.WriteString("`" + f.Formula + "`\n\n")
		b.WriteString("#v(0.2cm)\n")
	}
	b.WriteString("\n== Methodology\n\n")
	b.WriteString("Entity extraction is AI-assisted; all financial calculations are performed ")
	b.WriteString("deterministically. This report reflects data as of the generation date.\n")
	return b.String()
}

// renderTypstReport writes the Typst source to a temp file, runs `typst compile`,
// and moves the resulting PDF into the reports dir. Returns the relative storage
// path (or "" and an error if typst is unavailable or the render fails).
func renderTypstReport(d *reportData, reportID string) (string, error) {
	typstBin, err := exec.LookPath("typst")
	if err != nil {
		return "", err // typst not installed — metadata-only report
	}

	src := buildTypstSource(d)

	tmp, err := os.CreateTemp("", "report-*.typ")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(src); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	reportsDir := os.Getenv("REPORTS_DIR")
	if reportsDir == "" {
		reportsDir = "reports"
	}
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		return "", err
	}

	pdfPath := filepath.Join(reportsDir, reportID+".pdf")
	cmd := exec.Command(typstBin, "compile", tmp.Name(), pdfPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("typst compile: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return pdfPath, nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
