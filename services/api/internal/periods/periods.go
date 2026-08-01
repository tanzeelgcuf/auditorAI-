package periods

import (
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

const (
	errNotFound   = "https://ai-auditor.dev/errors/not-found"
	errBadRequest = "https://ai-auditor.dev/errors/bad-request"
	errConflict   = "https://ai-auditor.dev/errors/conflict"
	errForbidden  = "https://ai-auditor.dev/errors/forbidden"
	errInternal   = "https://ai-auditor.dev/errors/internal"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

// ---- helpers ----

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

// conn returns the RLS-wired request connection; nil means the handler is not
// behind RLSInjector, which every protected handler is.
func conn(r *http.Request) *pgxpool.Conn {
	return middleware.GetConn(r.Context())
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

type period struct {
	ID                      string    `json:"id"`
	ClientBookID            string    `json:"client_book_id"`
	PeriodStart             string    `json:"period_start"`
	PeriodEnd               string    `json:"period_end"`
	Status                  string    `json:"status"`
	TrialBalanceDebitsCents *int64    `json:"trial_balance_debits_cents"`
	TrialBalanceCreditsCents *int64   `json:"trial_balance_credits_cents"`
	TrialBalanceIsBalanced  *bool     `json:"trial_balance_is_balanced"`
	ClosedBy                *string   `json:"closed_by"`
	ClosedAt                *string   `json:"closed_at"`
}

const periodSelect = `SELECT id::text, client_book_id::text,
	to_char(period_start,'YYYY-MM-DD'), to_char(period_end,'YYYY-MM-DD'), status,
	trial_balance_debits_cents, trial_balance_credits_cents, trial_balance_is_balanced,
	closed_by::text, to_char(closed_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scanPeriod(row pgx.Row) (period, error) {
	var p period
	var closedAt *string
	err := row.Scan(&p.ID, &p.ClientBookID, &p.PeriodStart, &p.PeriodEnd, &p.Status,
		&p.TrialBalanceDebitsCents, &p.TrialBalanceCreditsCents, &p.TrialBalanceIsBalanced,
		&p.ClosedBy, &closedAt)
	if closedAt != nil {
		p.ClosedAt = closedAt
	}
	return p, err
}

// getPeriodWithBook fetches the period (with book assignment 404 via the
// assigned-books check) and returns its current status.
func (s *Service) getPeriodWithBook(r *http.Request, bookID, periodID string) (period, error) {
	assigned := middleware.GetAssignedBooks(r.Context())
	if !contains(assigned, bookID) {
		return period{}, errPeriodNotFound
	}
	c := conn(r)
	if c == nil {
		return period{}, errNoConn
	}
	p, err := scanPeriod(c.QueryRow(r.Context(),
		periodSelect+` FROM reconciliation_periods WHERE client_book_id = $1 AND id = $2`,
		bookID, periodID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return period{}, errPeriodNotFound
		}
		slog.Error("failed to fetch period", "error", err)
		return period{}, errNoConn
	}
	return p, nil
}

var (
	errPeriodNotFound = errors.New("period not found")
	errNoConn         = errors.New("no db conn")
)

// ---- HandleListPeriods ----

func (s *Service) HandleListPeriods(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errNotFound, "book not found")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	rows, err := c.Query(r.Context(),
		periodSelect+` FROM reconciliation_periods WHERE client_book_id = $1 ORDER BY period_start DESC`,
		bookID)
	if err != nil {
		slog.Error("failed to list periods", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	defer rows.Close()
	out := []period{}
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			slog.Warn("failed to scan period row", "error", err)
			continue
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

// ---- HandleCreatePeriod ----

func (s *Service) HandleCreatePeriod(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errNotFound, "book not found")
		return
	}
	var req struct {
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "invalid body")
		return
	}
	start, ok := parseDate(req.PeriodStart)
	if !ok {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "period_start must be YYYY-MM-DD")
		return
	}
	end, ok := parseDate(req.PeriodEnd)
	if !ok {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "period_end must be YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "period_end before period_start")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}

	// Reject if the period overlaps an existing period for this book.
	var overlap bool
	err := c.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM reconciliation_periods
		 WHERE client_book_id = $1 AND tstzrange(period_start, period_end, '[]') && tstzrange($2::date, $3::date, '[]'))`,
		bookID, start, end).Scan(&overlap)
	if err != nil {
		slog.Error("failed to check period overlap", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	if overlap {
		writeProblem(w, http.StatusConflict, errConflict, "period overlaps an existing period for this book")
		return
	}

	var newID string
	err = c.QueryRow(r.Context(),
		`INSERT INTO reconciliation_periods (client_book_id, period_start, period_end, status)
		 VALUES ($1, $2, $3, 'open') RETURNING id::text`,
		bookID, start, end).Scan(&newID)
	if err != nil {
		slog.Error("failed to create period", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "insert failed")
		return
	}

	p, err := scanPeriod(c.QueryRow(r.Context(),
		periodSelect+` FROM reconciliation_periods WHERE id = $1`, newID))
	if err != nil {
		slog.Error("failed to load created period", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "load failed")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ---- HandleClosePeriod (doc 10 §1) ----

func (s *Service) HandleClosePeriod(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	periodID := r.PathValue("periodId")
	if middleware.GetRole(r.Context()) != "firm_admin" {
		writeProblem(w, http.StatusForbidden, errForbidden, "firm_admin required")
		return
	}
	p, err := s.getPeriodWithBook(r, bookID, periodID)
	if err == errPeriodNotFound {
		writeProblem(w, http.StatusNotFound, errNotFound, "period not found")
		return
	}
	if err == errNoConn {
		writeProblem(w, http.StatusInternalServerError, errInternal, "internal error")
		return
	}
	if p.Status != "open" && p.Status != "pending_close" {
		writeProblem(w, http.StatusConflict, errConflict, fmt.Sprintf("cannot close period in status %s", p.Status))
		return
	}

	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}

	// Blocked by open document requests (doc 10 §4) — must be waived first.
	var pending int
	err = c.QueryRow(r.Context(),
		`SELECT count(*) FROM document_requests
		 WHERE reconciliation_period_id = $1 AND status = 'pending'`,
		periodID).Scan(&pending)
	if err != nil {
		slog.Error("failed to count document requests", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	if pending > 0 {
		writeProblem(w, http.StatusConflict, errConflict,
			fmt.Sprintf("cannot close period: %d pending document request(s) must be waived or fulfilled first", pending))
		return
	}

	userID := middleware.GetUserID(r.Context())

	// Trial balance validation: sum GL debits vs credits within the period.
	var debits, credits *int64
	var balanced *bool
	err = c.QueryRow(r.Context(),
		`SELECT NULLIF(sum(amount_cents) FILTER (WHERE debit_or_credit = 'debit'), 0),
			NULLIF(sum(amount_cents) FILTER (WHERE debit_or_credit = 'credit'), 0)
		 FROM extracted_entities
		 WHERE client_book_id = $1 AND entity_type = 'gl_entry'
		   AND entity_subtype IS DISTINCT FROM 'void'
		   AND transaction_date >= $2 AND transaction_date <= $3`,
		bookID, p.PeriodStart, p.PeriodEnd).Scan(&debits, &credits)
	if err != nil {
		slog.Error("failed to compute trial balance", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	hasGL := debits != nil || credits != nil
	if hasGL && debits != nil && credits != nil && *debits == *credits {
		b := true
		balanced = &b
	} else if hasGL {
		b := false
		balanced = &b
	}
	// No GL data: leave trial_balance_* NULL.

	// Recompute vendor spend baselines for the book (doc 10 §5): per
	// counterparty, avg/stddev of amount_cents over the last 6 periods.
	baselineRows, err := c.Query(r.Context(),
		`INSERT INTO vendor_spend_baselines (client_book_id, counterparty_canonical_name,
			trailing_avg_cents, trailing_stddev_cents, computed_through_period_id)
		SELECT $1, e.counterparty,
			round(avg(e.amount_cents)),
			round(stddev_pop(e.amount_cents)),
			$2
		FROM extracted_entities e
		JOIN reconciliation_periods p
		  ON p.client_book_id = e.client_book_id
		 AND e.transaction_date >= p.period_start
		 AND e.transaction_date <= p.period_end
		WHERE e.client_book_id = $1
		  AND e.counterparty IS NOT NULL
		  AND e.entity_subtype IS DISTINCT FROM 'void'
		  AND p.id IN (
			SELECT id FROM reconciliation_periods
			WHERE client_book_id = $1 AND period_end <= $3
			ORDER BY period_end DESC LIMIT 6)
		GROUP BY e.counterparty
		ON CONFLICT (client_book_id, counterparty_canonical_name)
		DO UPDATE SET trailing_avg_cents = EXCLUDED.trailing_avg_cents,
			trailing_stddev_cents = EXCLUDED.trailing_stddev_cents,
			computed_through_period_id = EXCLUDED.computed_through_period_id`,
		bookID, periodID, p.PeriodEnd)
	if err != nil {
		slog.Error("failed to recompute vendor baselines", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	baselineRows.Close()

	_, err = c.Exec(r.Context(),
		`UPDATE reconciliation_periods
		 SET status = 'pending_close', closed_by = $1, closed_at = now(),
			trial_balance_debits_cents = $2, trial_balance_credits_cents = $3,
			trial_balance_is_balanced = $4
		 WHERE id = $5`,
		userID, debits, credits, balanced, periodID)
	if err != nil {
		slog.Error("failed to update period status", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "update failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "close_period", periodID)

	updated, err := scanPeriod(c.QueryRow(r.Context(),
		periodSelect+` FROM reconciliation_periods WHERE id = $1`, periodID))
	if err != nil {
		slog.Error("failed to fetch closed period", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ---- HandleReopenPeriod (doc 10 §1) ----

func (s *Service) HandleReopenPeriod(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	periodID := r.PathValue("periodId")
	if middleware.GetRole(r.Context()) != "firm_admin" {
		writeProblem(w, http.StatusForbidden, errForbidden, "firm_admin required")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "reason is required — reopen is an accountability action")
		return
	}
	p, err := s.getPeriodWithBook(r, bookID, periodID)
	if err == errPeriodNotFound {
		writeProblem(w, http.StatusNotFound, errNotFound, "period not found")
		return
	}
	if err == errNoConn {
		writeProblem(w, http.StatusInternalServerError, errInternal, "internal error")
		return
	}
	if p.Status != "pending_close" && p.Status != "closed" {
		writeProblem(w, http.StatusConflict, errConflict, fmt.Sprintf("cannot reopen period in status %s", p.Status))
		return
	}

	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	userID := middleware.GetUserID(r.Context())

	// On reopen, the period re-enters active workflow: clear the close markers
	// and drop the stale trial balance figures.
	_, err = c.Exec(r.Context(),
		`UPDATE reconciliation_periods
		 SET status = 'reopened', closed_by = NULL, closed_at = NULL,
			trial_balance_debits_cents = NULL, trial_balance_credits_cents = NULL,
			trial_balance_is_balanced = NULL
		 WHERE id = $1`, periodID)
	if err != nil {
		slog.Error("failed to reopen period", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "update failed")
		return
	}
	_, err = c.Exec(r.Context(),
		`INSERT INTO period_reopen_log (reconciliation_period_id, reopened_by, reason)
		 VALUES ($1, $2, $3)`,
		periodID, userID, req.Reason)
	if err != nil {
		slog.Error("failed to log reopen", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "insert failed")
		return
	}

	middleware.RecordAccess(r.Context(), s.db, userID, bookID, "reopen_period", periodID)

	updated, err := scanPeriod(c.QueryRow(r.Context(),
		periodSelect+` FROM reconciliation_periods WHERE id = $1`, periodID))
	if err != nil {
		slog.Error("failed to fetch reopened period", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ---- Document requests ----

type docRequest struct {
	ID                   string `json:"id"`
	ClientBookID         string `json:"client_book_id"`
	ReconciliationPeriodID *string `json:"reconciliation_period_id"`
	RequestedDocType     string `json:"requested_doc_type"`
	Description          *string `json:"description"`
	RequestedBy          string `json:"requested_by"`
	Status               string `json:"status"`
	RequestedAt          string `json:"requested_at"`
	ReminderSentCount    int    `json:"reminder_sent_count"`
}

func scanDocRequest(row pgx.Row) (docRequest, error) {
	var d docRequest
	var periodID *string
	var desc *string
	var requestedAt string
	err := row.Scan(&d.ID, &d.ClientBookID, &periodID, &d.RequestedDocType, &desc,
		&d.RequestedBy, &d.Status, &requestedAt, &d.ReminderSentCount)
	if periodID != nil {
		d.ReconciliationPeriodID = periodID
	}
	if desc != nil {
		d.Description = desc
	}
	d.RequestedAt = requestedAt
	return d, err
}

const docReqSelect = `SELECT id::text, client_book_id::text, reconciliation_period_id::text,
	requested_doc_type, description, requested_by::text, status,
	to_char(requested_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'), reminder_sent_count`

func (s *Service) HandleListDocumentRequests(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errNotFound, "book not found")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	status := r.URL.Query().Get("status")
	query := docReqSelect + ` FROM document_requests WHERE client_book_id = $1`
	args := []interface{}{bookID}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY requested_at DESC"
	rows, err := c.Query(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to list document requests", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	defer rows.Close()
	out := []docRequest{}
	for rows.Next() {
		d, err := scanDocRequest(rows)
		if err != nil {
			slog.Warn("failed to scan doc request row", "error", err)
			continue
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateDocumentRequest(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errNotFound, "book not found")
		return
	}
	var req struct {
		ReconciliationPeriodID *string `json:"reconciliation_period_id"`
		RequestedDocType       string  `json:"requested_doc_type"`
		Description            *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.RequestedDocType) == "" {
		writeProblem(w, http.StatusBadRequest, errBadRequest, "requested_doc_type is required")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	var id, requestedAt string
	err := c.QueryRow(r.Context(),
		`INSERT INTO document_requests (client_book_id, reconciliation_period_id, requested_doc_type, description, requested_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id::text, to_char(requested_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		bookID, req.ReconciliationPeriodID, req.RequestedDocType, req.Description,
		middleware.GetUserID(r.Context())).Scan(&id, &requestedAt)
	if err != nil {
		slog.Error("failed to create document request", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": id, "client_book_id": bookID, "requested_at": requestedAt,
		"status": "pending",
	})
}

func (s *Service) HandleWaiveDocumentRequest(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	requestID := r.PathValue("requestId")
	if middleware.GetRole(r.Context()) != "firm_admin" {
		writeProblem(w, http.StatusForbidden, errForbidden, "firm_admin required")
		return
	}
	if !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errNotFound, "book not found")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	tag, err := c.Exec(r.Context(),
		`UPDATE document_requests SET status = 'waived'
		 WHERE id = $1 AND client_book_id = $2 AND status = 'pending'`,
		requestID, bookID)
	if err != nil {
		slog.Error("failed to waive document request", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		// Not found, not in this book, or not pending — all surface as 404/409.
		var exists bool
		_ = c.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM document_requests WHERE id = $1 AND client_book_id = $2)`,
			requestID, bookID).Scan(&exists)
		if !exists {
			writeProblem(w, http.StatusNotFound, errNotFound, "document request not found")
			return
		}
		writeProblem(w, http.StatusConflict, errConflict, "only pending requests can be waived")
		return
	}
	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), bookID, "waive_document_request", requestID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "document request waived"})
}

// ---- HandleFirmDashboard (doc 08 §6) ----

func (s *Service) HandleFirmDashboard(w http.ResponseWriter, r *http.Request) {
	if middleware.GetRole(r.Context()) != "firm_admin" {
		writeProblem(w, http.StatusForbidden, errForbidden, "firm_admin required")
		return
	}
	c := conn(r)
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errInternal, "no db conn")
		return
	}
	firmID := middleware.GetFirmID(r.Context())

	// Open findings by severity.
	var totalOpen int
	bySeverity := map[string]int{"info": 0, "low": 0, "medium": 0, "high": 0}
	rows, err := c.Query(r.Context(),
		`SELECT severity, count(*)
		 FROM audit_findings
		 WHERE status = 'open'
		   AND client_book_id IN (SELECT id FROM client_books WHERE firm_id = $1)
		 GROUP BY severity`, firmID)
	if err != nil {
		slog.Error("failed to aggregate findings", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			continue
		}
		bySeverity[sev] = n
		totalOpen += n
	}
	rows.Close()

	// Past-due findings: severity-based business-day due window, computed in SQL.
	var pastDue int
	err = c.QueryRow(r.Context(),
		`SELECT count(*) FROM audit_findings
		 WHERE status = 'open'
		   AND client_book_id IN (SELECT id FROM client_books WHERE firm_id = $1)
		   AND created_at::date +
			  CASE severity WHEN 'high' THEN 2 WHEN 'medium' THEN 7 WHEN 'low' THEN 30 ELSE 0 END
			  + 2 * (CASE severity WHEN 'high' THEN 2 WHEN 'medium' THEN 7 WHEN 'low' THEN 30 ELSE 0 END) / 7
		   < now()`,
		firmID).Scan(&pastDue)
	if err != nil {
		slog.Error("failed to count past-due findings", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}

	// Average time-to-resolution over the last 30 days (in seconds).
	var avgResolution *float64
	err = c.QueryRow(r.Context(),
		`SELECT avg(EXTRACT(EPOCH FROM (reviewed_at - created_at)))
		 FROM audit_findings
		 WHERE reviewed_at IS NOT NULL
		   AND client_book_id IN (SELECT id FROM client_books WHERE firm_id = $1)
		   AND reviewed_at >= now() - interval '30 days'`,
		firmID).Scan(&avgResolution)
	if err != nil {
		slog.Error("failed to compute resolution time", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}

	// Per-book breakdown (open + past-due per book).
	type bookStat struct {
		ID           string `json:"id"`
		ClientName   string `json:"client_name"`
		OpenFindings int    `json:"open_findings"`
		PastDue      int    `json:"past_due"`
	}
	rows, err = c.Query(r.Context(),
		`SELECT cb.id::text, cb.client_name, count(f.id),
			count(*) FILTER (WHERE f.created_at::date +
				CASE f.severity WHEN 'high' THEN 2 WHEN 'medium' THEN 7 WHEN 'low' THEN 30 ELSE 0 END
				+ 2 * (CASE f.severity WHEN 'high' THEN 2 WHEN 'medium' THEN 7 WHEN 'low' THEN 30 ELSE 0 END) / 7
				< now())
		 FROM client_books cb
		 LEFT JOIN audit_findings f ON f.client_book_id = cb.id AND f.status = 'open'
		 WHERE cb.firm_id = $1
		 GROUP BY cb.id, cb.client_name
		 ORDER BY cb.client_name`, firmID)
	if err != nil {
		slog.Error("failed to aggregate per-book findings", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}
	booksOut := []bookStat{}
	for rows.Next() {
		var b bookStat
		if err := rows.Scan(&b.ID, &b.ClientName, &b.OpenFindings, &b.PastDue); err != nil {
			continue
		}
		booksOut = append(booksOut, b)
	}
	rows.Close()

	// Stale requests: requests that have had 3+ reminders (doc 10 §7).
	var staleRequests int
	err = c.QueryRow(r.Context(),
		`SELECT count(*) FROM document_requests
		 WHERE status = 'pending' AND reminder_sent_count >= 3
		   AND client_book_id IN (SELECT id FROM client_books WHERE firm_id = $1)`,
		firmID).Scan(&staleRequests)
	if err != nil {
		slog.Error("failed to count stale requests", "error", err)
		writeProblem(w, http.StatusInternalServerError, errInternal, "query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_open_findings":   totalOpen,
		"open_by_severity":      bySeverity,
		"past_due_findings":     pastDue,
		"avg_resolution_time":   avgResolution,
		"stale_requests":        staleRequests,
		"books":                 booksOut,
	})
}
