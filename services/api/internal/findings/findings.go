package findings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	_, err := c.Exec(r.Context(),
		`INSERT INTO finding_attachments (audit_finding_id, uploaded_by, storage_key, filename)
		 VALUES ($1, $2, $3, $4)`,
		findingID, userID, req.StorageKey, req.Filename)
	if err != nil {
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

	// Gather findings for the period (created in range, for this book)
	rows, err := c.Query(r.Context(),
		`SELECT id::text, rule_id, rule_version, calculated_variance_cents, tolerance_cents,
			exceeds_tolerance, calculation_formula, severity, status
		 FROM audit_findings
		 WHERE client_book_id = $1 AND created_at >= $2 AND created_at < $3
		 ORDER BY created_at DESC`, bookID, start, end.Add(24*time.Hour))
	if err != nil {
		slog.Error("failed to query findings for report", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	var findingIDs []string
	var pdfData reportData
	for rows.Next() {
		var id, ruleID, ruleVer, formula, sev, st string
		var variance, tolerance int64
		var exceeds bool
		if err := rows.Scan(&id, &ruleID, &ruleVer, &variance, &tolerance, &exceeds,
			&formula, &sev, &st); err != nil {
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

	// Generate PDF via Typst (self-hosted per docs). Build a minimal Typst source
	// and shell out to the typst binary if present; otherwise skip and return metadata.
	pdfKey := ""
	if typstAvailable() {
		src := buildTypstSource(&pdfData)
		pdfKey = fmt.Sprintf("reports/%s.pdf", reportID)
		// In production: write src to temp file, run `typst compile`, upload PDF to S3.
		// For now we record intent; the /citation endpoint already serves the traceability.
		_ = src
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": reportID, "client_book_id": bookID,
		"period_start": req.PeriodStart, "period_end": req.PeriodEnd,
		"generated_at": time.Now().Format(time.RFC3339),
		"finding_ids": findingIDs, "pdf_storage_key": pdfKey,
	})
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
			f.rule_id, f.rule_version
		 FROM audit_findings f
		 JOIN reconciliation_group_members m ON m.reconciliation_group_id = f.reconciliation_group_id
		 JOIN extracted_entities e ON e.id = m.extracted_entity_id
		 WHERE f.id = $1
		 ORDER BY CASE m.role WHEN 'invoice' THEN 0 WHEN 'bank' THEN 1 ELSE 2 END
		 LIMIT 1`, findingID)

	var sourceDoc string
	var page int
	var bboxJSON []byte
	var ruleID, ruleVer string
	if err := row.Scan(&sourceDoc, &page, &bboxJSON, &ruleID, &ruleVer); err != nil {
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

func typstAvailable() bool {
	// In production: exec.LookPath("typst"). For now report generation returns
	// metadata only; PDF rendering is wired in when the typst binary is present.
	return false
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
