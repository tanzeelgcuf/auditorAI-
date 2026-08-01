// Package settings implements the "settings layer" for AI Auditor v1:
// chart of accounts, counterparty aliases, CSV column mappings, and
// firm-level API keys + webhooks.
//
// Multi-tenancy: book-scoped handlers verify the book is in
// middleware.GetAssignedBooks (404 for cross-book, never 403), then run
// queries on the RLS-wired request connection. Firm-scoped admin handlers
// rely on the RLS policies keyed on app.current_firm.
package settings

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

const (
	errTypeNotFound     = "https://ai-auditor.dev/errors/not-found"
	errTypeBadRequest   = "https://ai-auditor.dev/errors/bad-request"
	errTypeConflict     = "https://ai-auditor.dev/errors/conflict"
	errTypeInternal     = "https://ai-auditor.dev/errors/internal"
	errTypeUnauthorized = "https://ai-auditor.dev/errors/unauthorized"

	apiKeyPrefix = "aiaud_"
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

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// conn returns the RLS-wired request connection, falling back to a dedicated
// pool connection when the middleware did not provide one.
func (s *Service) conn(r *http.Request) (pgxQuerier, func(), error) {
	if c := middleware.GetConn(r.Context()); c != nil {
		return c, func() {}, nil
	}
	c, err := s.db.Acquire(r.Context())
	if err != nil {
		return nil, nil, err
	}
	return c, c.Release, nil
}

// pgxQuerier abstracts the connection types used for single-statement queries.
type pgxQuerier interface {
	Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// bookAllowed returns the bookId from the path if the caller is assigned to
// it; cross-book requests get a 404 (RFC 7807) and return "".
func bookAllowed(w http.ResponseWriter, r *http.Request) string {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "book not found")
		return ""
	}
	return bookID
}

// ---- pure helpers (tested) ----

// defaultReconcilable decides is_reconcilable when the caller omits it:
// equity accounts default to false (retained earnings / owner draws are not
// individually reconcilable), everything else to true.
func defaultReconcilable(accountType string, provided *bool) bool {
	if provided != nil {
		return *provided
	}
	return accountType != "equity"
}

func validAccountType(t string) bool {
	switch t {
	case "asset", "liability", "equity", "revenue", "expense":
		return true
	}
	return false
}

// GenerateAPIKey returns a key of the form aiaud_<32 hex chars>.
func GenerateAPIKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return apiKeyPrefix + hex.EncodeToString(buf), nil
}

// ValidateAPIKeyFormat checks the aiaud_ + 32 hex chars shape. (Real keys are
// validated against the DB by AuthAPIKey; this guards the shape.)
func ValidateAPIKeyFormat(key string) bool {
	if !strings.HasPrefix(key, apiKeyPrefix) || len(key) != len(apiKeyPrefix)+32 {
		return false
	}
	_, err := hex.DecodeString(key[len(apiKeyPrefix):])
	return err == nil
}

// GenerateSecret returns 32 hex chars for webhook signing secrets.
func GenerateSecret() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HashKey returns the hex SHA-256 of a raw API key. Only the hash is stored.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// SignHMAC returns hex(HMAC-SHA256(secret, body)) — the webhook signature
// sent as the X-AI-Auditor-Signature header.
func SignHMAC(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// =====================================================================
// Chart of Accounts
// =====================================================================

type chartAccount struct {
	ID             string `json:"id"`
	ClientBookID   string `json:"client_book_id"`
	AccountCode    string `json:"account_code"`
	AccountName    string `json:"account_name"`
	AccountType    string `json:"account_type"`
	IsReconcilable bool   `json:"is_reconcilable"`
}

func (s *Service) HandleListChartOfAccounts(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		slog.Error("failed to acquire connection", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, client_book_id::text, account_code, account_name, account_type, is_reconcilable
		 FROM chart_of_accounts WHERE client_book_id = $1 ORDER BY account_code`, bookID)
	if err != nil {
		slog.Error("failed to list chart of accounts", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []chartAccount{}
	for rows.Next() {
		var a chartAccount
		if err := rows.Scan(&a.ID, &a.ClientBookID, &a.AccountCode, &a.AccountName, &a.AccountType, &a.IsReconcilable); err != nil {
			continue
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateChartAccount(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}

	var req struct {
		AccountCode    string `json:"account_code"`
		AccountName    string `json:"account_name"`
		AccountType    string `json:"account_type"`
		IsReconcilable *bool  `json:"is_reconcilable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if req.AccountCode == "" || req.AccountName == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "account_code and account_name are required")
		return
	}
	if !validAccountType(req.AccountType) {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "account_type must be asset, liability, equity, revenue, or expense")
		return
	}
	reconcilable := defaultReconcilable(req.AccountType, req.IsReconcilable)

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var a chartAccount
	err = q.QueryRow(r.Context(),
		`INSERT INTO chart_of_accounts (client_book_id, account_code, account_name, account_type, is_reconcilable)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text, client_book_id::text, account_code, account_name, account_type, is_reconcilable`,
		bookID, req.AccountCode, req.AccountName, req.AccountType, reconcilable).
		Scan(&a.ID, &a.ClientBookID, &a.AccountCode, &a.AccountName, &a.AccountType, &a.IsReconcilable)
	if err != nil {
		if isUniqueViolation(err) {
			writeProblem(w, http.StatusConflict, errTypeConflict, "account_code already exists for this book")
			return
		}
		slog.Error("failed to create chart account", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Service) HandleUpdateChartAccount(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	accountID := r.PathValue("accountId")
	if accountID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "accountId required")
		return
	}

	var req struct {
		AccountName    *string `json:"account_name"`
		IsReconcilable *bool   `json:"is_reconcilable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if req.AccountName == nil && req.IsReconcilable == nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "no fields to update")
		return
	}

	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1
	if req.AccountName != nil {
		setClauses = append(setClauses, "account_name = $"+itoa(argIdx))
		args = append(args, *req.AccountName)
		argIdx++
	}
	if req.IsReconcilable != nil {
		setClauses = append(setClauses, "is_reconcilable = $"+itoa(argIdx))
		args = append(args, *req.IsReconcilable)
		argIdx++
	}
	args = append(args, bookID, accountID)
	query := "UPDATE chart_of_accounts SET " + strings.Join(setClauses, ", ") +
		" WHERE client_book_id = $" + itoa(argIdx) + " AND id = $" + itoa(argIdx+1)

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	tag, err := q.Exec(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to update chart account", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "account updated"})
}

// ---- COA templates ----

type coaTemplate struct {
	ID           string          `json:"id"`
	TemplateName string          `json:"template_name"`
	Industry     *string         `json:"industry"`
	Accounts     json.RawMessage `json:"accounts"`
}

func (s *Service) HandleListCOATemplates(w http.ResponseWriter, r *http.Request) {
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, template_name, industry, accounts FROM coa_templates ORDER BY template_name`)
	if err != nil {
		slog.Error("failed to list coa templates", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []coaTemplate{}
	for rows.Next() {
		var t coaTemplate
		var accounts []byte
		if err := rows.Scan(&t.ID, &t.TemplateName, &t.Industry, &accounts); err != nil {
			continue
		}
		t.Accounts = json.RawMessage(accounts)
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

type templateAccount struct {
	AccountCode    string `json:"account_code"`
	AccountName    string `json:"account_name"`
	AccountType    string `json:"account_type"`
	IsReconcilable bool   `json:"is_reconcilable"`
}

func (s *Service) HandleApplyTemplate(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}

	var req struct {
		TemplateID string `json:"template_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TemplateID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "template_id is required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "no db conn")
		return
	}
	tx, err := c.Begin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer tx.Rollback(r.Context())

	var accounts []byte
	err = tx.QueryRow(r.Context(),
		`SELECT accounts FROM coa_templates WHERE id = $1`, req.TemplateID).Scan(&accounts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, errTypeNotFound, "template not found")
			return
		}
		slog.Error("failed to fetch template", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}

	var tpl []templateAccount
	if err := json.Unmarshal(accounts, &tpl); err != nil {
		slog.Error("failed to parse template accounts", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "invalid template data")
		return
	}

	// Upsert each account; existing codes for the book are left untouched
	// (DO NOTHING) so a template never clobbers book-specific edits.
	inserted := 0
	for _, a := range tpl {
		if a.AccountCode == "" || a.AccountName == "" || !validAccountType(a.AccountType) {
			continue
		}
		tag, err := tx.Exec(r.Context(),
			`INSERT INTO chart_of_accounts (client_book_id, account_code, account_name, account_type, is_reconcilable)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (client_book_id, account_code) DO NOTHING`,
			bookID, a.AccountCode, a.AccountName, a.AccountType, a.IsReconcilable)
		if err != nil {
			slog.Error("failed to apply template account", "error", err)
			writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
			return
		}
		inserted += int(tag.RowsAffected())
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("failed to commit template apply", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "commit failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"applied": inserted})
}

// =====================================================================
// Counterparty aliases
// =====================================================================

type counterpartyAlias struct {
	ID            string  `json:"id"`
	ClientBookID  string  `json:"client_book_id"`
	CanonicalName string  `json:"canonical_name"`
	Alias         string  `json:"alias"`
	ConfirmedBy   *string `json:"confirmed_by"`
}

func (s *Service) HandleListAliases(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, client_book_id::text, canonical_name, alias, confirmed_by::text
		 FROM counterparty_aliases WHERE client_book_id = $1 ORDER BY canonical_name, alias`, bookID)
	if err != nil {
		slog.Error("failed to list aliases", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []counterpartyAlias{}
	for rows.Next() {
		var a counterpartyAlias
		if err := rows.Scan(&a.ID, &a.ClientBookID, &a.CanonicalName, &a.Alias, &a.ConfirmedBy); err != nil {
			continue
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateAlias(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}

	var req struct {
		CanonicalName string  `json:"canonical_name"`
		Alias         string  `json:"alias"`
		ConfirmedBy   *string `json:"confirmed_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if req.CanonicalName == "" || req.Alias == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "canonical_name and alias are required")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var a counterpartyAlias
	err = q.QueryRow(r.Context(),
		`INSERT INTO counterparty_aliases (client_book_id, canonical_name, alias, confirmed_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id::text, client_book_id::text, canonical_name, alias, confirmed_by::text`,
		bookID, req.CanonicalName, req.Alias, req.ConfirmedBy).
		Scan(&a.ID, &a.ClientBookID, &a.CanonicalName, &a.Alias, &a.ConfirmedBy)
	if err != nil {
		if isUniqueViolation(err) {
			writeProblem(w, http.StatusConflict, errTypeConflict, "alias already exists for this book")
			return
		}
		slog.Error("failed to create alias", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Service) HandleDeleteAlias(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	aliasID := r.PathValue("aliasId")
	if aliasID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "aliasId required")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	tag, err := q.Exec(r.Context(),
		`DELETE FROM counterparty_aliases WHERE client_book_id = $1 AND id = $2`, bookID, aliasID)
	if err != nil {
		slog.Error("failed to delete alias", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "alias not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "alias deleted"})
}

// =====================================================================
// CSV column mappings
// =====================================================================

type csvMapping struct {
	ID           string                 `json:"id"`
	ClientBookID string                 `json:"client_book_id"`
	SourceSystem *string                `json:"source_system"`
	ColumnMap    map[string]interface{} `json:"column_map"`
	CreatedAt    time.Time              `json:"created_at"`
}

func (s *Service) HandleListCSVMappings(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, client_book_id::text, source_system, column_map, created_at
		 FROM csv_column_mappings WHERE client_book_id = $1 ORDER BY created_at DESC`, bookID)
	if err != nil {
		slog.Error("failed to list csv mappings", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []csvMapping{}
	for rows.Next() {
		var m csvMapping
		var cm []byte
		if err := rows.Scan(&m.ID, &m.ClientBookID, &m.SourceSystem, &cm, &m.CreatedAt); err != nil {
			continue
		}
		m.ColumnMap = map[string]interface{}{}
		if len(cm) > 0 {
			_ = json.Unmarshal(cm, &m.ColumnMap)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateCSVMapping(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}

	var req struct {
		SourceSystem *string                `json:"source_system"`
		ColumnMap    map[string]interface{} `json:"column_map"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if len(req.ColumnMap) == 0 {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "column_map is required")
		return
	}
	cmJSON, err := json.Marshal(req.ColumnMap)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid column_map")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var id string
	err = q.QueryRow(r.Context(),
		`INSERT INTO csv_column_mappings (client_book_id, source_system, column_map)
		 VALUES ($1, $2, $3) RETURNING id::text`,
		bookID, req.SourceSystem, cmJSON).Scan(&id)
	if err != nil {
		slog.Error("failed to create csv mapping", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Service) HandleUpdateCSVMapping(w http.ResponseWriter, r *http.Request) {
	bookID := bookAllowed(w, r)
	if bookID == "" {
		return
	}
	mappingID := r.PathValue("mappingId")
	if mappingID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "mappingId required")
		return
	}

	var req struct {
		SourceSystem *string                `json:"source_system"`
		ColumnMap    map[string]interface{} `json:"column_map"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if len(req.ColumnMap) == 0 {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "column_map is required")
		return
	}
	cmJSON, err := json.Marshal(req.ColumnMap)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid column_map")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	tag, err := q.Exec(r.Context(),
		`UPDATE csv_column_mappings SET source_system = $1, column_map = $2
		 WHERE client_book_id = $3 AND id = $4`,
		req.SourceSystem, cmJSON, bookID, mappingID)
	if err != nil {
		slog.Error("failed to update csv mapping", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "mapping not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "mapping updated"})
}

// =====================================================================
// API keys (firm_admin)
// =====================================================================

type apiKeyRow struct {
	ID         string     `json:"id"`
	KeyHash    string     `json:"key_hash"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func (s *Service) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, errTypeUnauthorized, "unauthorized")
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, key_hash, last_used_at, revoked_at FROM api_keys
		 WHERE firm_id = $1 ORDER BY created_at DESC`, firmID)
	if err != nil {
		slog.Error("failed to list api keys", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []apiKeyRow{}
	for rows.Next() {
		var k apiKeyRow
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.LastUsedAt, &k.RevokedAt); err != nil {
			continue
		}
		out = append(out, k)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	userID := middleware.GetUserID(r.Context())
	if firmID == "" || userID == "" {
		writeProblem(w, http.StatusUnauthorized, errTypeUnauthorized, "unauthorized")
		return
	}

	raw, err := GenerateAPIKey()
	if err != nil {
		slog.Error("failed to generate api key", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var id string
	err = q.QueryRow(r.Context(),
		`INSERT INTO api_keys (firm_id, key_hash, created_by) VALUES ($1, $2, $3) RETURNING id::text`,
		firmID, HashKey(raw), userID).Scan(&id)
	if err != nil {
		slog.Error("failed to insert api key", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
		return
	}

	// The raw key is returned exactly once; only its hash is persisted.
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "api_key": raw})
}

func (s *Service) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyId")
	if keyID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "keyId required")
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	tag, err := q.Exec(r.Context(),
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		slog.Error("failed to revoke api key", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "key revoked"})
}

// AuthAPIKey hashes a presented key and looks up a non-revoked match,
// returning the owning firm_id. Intended for future programmatic access
// (not yet wired to a route).
func AuthAPIKey(ctx context.Context, db *pgxpool.Pool, key string) (string, error) {
	var firmID string
	err := db.QueryRow(ctx,
		`SELECT firm_id::text FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		HashKey(key)).Scan(&firmID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errors.New("invalid api key")
		}
		return "", err
	}
	_, _ = db.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE key_hash = $1`, HashKey(key))
	return firmID, nil
}

// =====================================================================
// Webhooks (firm_admin)
// =====================================================================

type webhookRow struct {
	ID         string    `json:"id"`
	TargetURL  string    `json:"target_url"`
	EventTypes []string  `json:"event_types"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Service) HandleListWebhooks(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, errTypeUnauthorized, "unauthorized")
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	rows, err := q.Query(r.Context(),
		`SELECT id::text, target_url, event_types, created_at FROM webhook_subscriptions
		 WHERE firm_id = $1 ORDER BY created_at DESC`, firmID)
	if err != nil {
		slog.Error("failed to list webhooks", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}
	defer rows.Close()

	out := []webhookRow{}
	for rows.Next() {
		var w webhookRow
		if err := rows.Scan(&w.ID, &w.TargetURL, &w.EventTypes, &w.CreatedAt); err != nil {
			continue
		}
		out = append(out, w)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

func (s *Service) HandleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, errTypeUnauthorized, "unauthorized")
		return
	}

	var req struct {
		TargetURL  string   `json:"target_url"`
		EventTypes []string `json:"event_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid request body")
		return
	}
	if req.TargetURL == "" || len(req.EventTypes) == 0 {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "target_url and event_types are required")
		return
	}
	if !strings.HasPrefix(req.TargetURL, "https://") && !strings.HasPrefix(req.TargetURL, "http://") {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "target_url must be http(s)")
		return
	}

	secret, err := GenerateSecret()
	if err != nil {
		slog.Error("failed to generate webhook secret", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}

	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var id string
	err = q.QueryRow(r.Context(),
		`INSERT INTO webhook_subscriptions (firm_id, target_url, event_types, signing_secret)
		 VALUES ($1, $2, $3, $4) RETURNING id::text`,
		firmID, req.TargetURL, req.EventTypes, secret).Scan(&id)
	if err != nil {
		slog.Error("failed to insert webhook", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "insert failed")
		return
	}

	// Signing secret returned exactly once; only stored server-side thereafter.
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": id, "target_url": req.TargetURL, "event_types": req.EventTypes, "signing_secret": secret,
	})
}

func (s *Service) HandleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("webhookId")
	if webhookID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "webhookId required")
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	tag, err := q.Exec(r.Context(),
		`DELETE FROM webhook_subscriptions WHERE id = $1`, webhookID)
	if err != nil {
		slog.Error("failed to delete webhook", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, errTypeNotFound, "webhook not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "webhook deleted"})
}

func (s *Service) HandleTestWebhook(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("webhookId")
	if webhookID == "" {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "webhookId required")
		return
	}
	q, release, err := s.conn(r)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	defer release()

	var targetURL, secret string
	err = q.QueryRow(r.Context(),
		`SELECT target_url, signing_secret FROM webhook_subscriptions WHERE id = $1`,
		webhookID).Scan(&targetURL, &secret)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, errTypeNotFound, "webhook not found")
			return
		}
		slog.Error("failed to load webhook", "error", err)
		writeProblem(w, http.StatusInternalServerError, errTypeInternal, "query failed")
		return
	}

	body := []byte(`{"event":"ping"}`)
	req, err := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(string(body)))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errTypeBadRequest, "invalid target_url")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AI-Auditor-Signature", SignHMAC(secret, string(body)))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("webhook test ping failed", "error", err, "webhook_id", webhookID)
		writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/webhook-undeliverable", "target unreachable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("webhook test ping rejected", "status", resp.StatusCode, "webhook_id", webhookID)
		writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/webhook-undeliverable", "target rejected ping")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "ping delivered"})
}

func itoa(n int) string {
	return string(rune('0' + n))
}
