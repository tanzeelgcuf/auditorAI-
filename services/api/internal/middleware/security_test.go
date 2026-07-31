// Package middleware_test — Phase 5 multi-tenancy security hardening tests.
//
// These tests run against a REAL Postgres with the infra/init.sql schema and
// RLS policies applied. They skip cleanly when no test database is available:
// set DATABASE_URL_TEST (e.g. postgres://postgres@localhost:5432/ai_auditor_test)
// to run them locally. The CI workflow provisions a postgres service container.
package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/documents"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/findings"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/review"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/tenant"
)

// securityEnv holds the shared DB pool and the ids of all seeded entities.
type securityEnv struct {
	pool *pgxpool.Pool

	firmA, firmB         string
	adminA, adminB       string
	staffA               string
	bookA, bookA2, bookB string
	docA, docB           string
	groupA               string
	findingA             string
	reportA              string
}

func testDSN() string {
	return os.Getenv("DATABASE_URL_TEST")
}

// requireDB returns a pool to the test DB or skips the test.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set; skipping multi-tenancy security test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no test database available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database not reachable: %v", err)
	}
	return pool
}

func mustQueryRow(t *testing.T, pool *pgxpool.Pool, query string, args ...interface{}) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&id); err != nil {
		t.Fatalf("seed query failed (%s): %v", query, err)
	}
	return id
}

// setupEnv seeds two firms with books, users, assignments and cross-tenant
// sample data, then truncates the tenant tables so tests are hermetic.
func setupEnv(t *testing.T) *securityEnv {
	t.Helper()
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	// Truncate everything with tenant scope (CASCADE reaches the rest).
	if _, err := pool.Exec(ctx,
		`TRUNCATE firms, users, client_books, user_book_assignments, source_documents,
			extracted_entities, reconciliation_groups, audit_findings, audit_reports,
			access_log, data_encryption_keys CASCADE`); err != nil {
		t.Fatalf("failed to truncate test tables: %v", err)
	}

	env := &securityEnv{pool: pool}

	env.firmA = mustQueryRow(t, pool, `INSERT INTO firms (name) VALUES ('Firm A') RETURNING id::text`)
	env.firmB = mustQueryRow(t, pool, `INSERT INTO firms (name) VALUES ('Firm B') RETURNING id::text`)

	// Firm A users. email_verified = true so login flow works; password hashes are unused.
	env.adminA = mustQueryRow(t, pool,
		`INSERT INTO users (firm_id, email, password_hash, role, email_verified)
		 VALUES ($1, 'admin-a@test.local', 'unused', 'firm_admin', true) RETURNING id::text`, env.firmA)
	env.staffA = mustQueryRow(t, pool,
		`INSERT INTO users (firm_id, email, password_hash, role, email_verified)
		 VALUES ($1, 'staff-a@test.local', 'unused', 'staff', true) RETURNING id::text`, env.firmA)
	env.adminB = mustQueryRow(t, pool,
		`INSERT INTO users (firm_id, email, password_hash, role, email_verified)
		 VALUES ($1, 'admin-b@test.local', 'unused', 'firm_admin', true) RETURNING id::text`, env.firmB)

	// Books: A and A2 in firm A; B in firm B.
	env.bookA = mustQueryRow(t, pool, `INSERT INTO client_books (firm_id, client_name) VALUES ($1, 'Acme') RETURNING id::text`, env.firmA)
	env.bookA2 = mustQueryRow(t, pool, `INSERT INTO client_books (firm_id, client_name) VALUES ($1, 'Globex') RETURNING id::text`, env.firmA)
	env.bookB = mustQueryRow(t, pool, `INSERT INTO client_books (firm_id, client_name) VALUES ($1, 'Initech') RETURNING id::text`, env.firmB)

	// staffA is assigned only to bookA. adminA assigned to bookA (creates book pattern).
	_, err := pool.Exec(ctx,
		`INSERT INTO user_book_assignments (user_id, client_book_id) VALUES
		 ($1, $2), ($3, $4)`,
		env.staffA, env.bookA, env.adminA, env.bookA)
	if err != nil {
		t.Fatalf("failed to seed assignments: %v", err)
	}

	// A document in bookA and one in bookB.
	env.docA = mustQueryRow(t, pool,
		`INSERT INTO source_documents (client_book_id, filename, doc_type, storage_key, content_hash, uploaded_by)
		 VALUES ($1, 'invoice-a.pdf', 'invoice', 'k/a.pdf', 'hash-a', $2) RETURNING id::text`,
		env.bookA, env.staffA)
	env.docB = mustQueryRow(t, pool,
		`INSERT INTO source_documents (client_book_id, filename, doc_type, storage_key, content_hash, uploaded_by)
		 VALUES ($1, 'invoice-b.pdf', 'invoice', 'k/b.pdf', 'hash-b', $2) RETURNING id::text`,
		env.bookB, env.adminB)

	// A reconciliation group + finding + report in bookA.
	env.groupA = mustQueryRow(t, pool,
		`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status)
		 VALUES ($1, 0.9, 'needs_review') RETURNING id::text`, env.bookA)
	env.findingA = mustQueryRow(t, pool,
		`INSERT INTO audit_findings (client_book_id, reconciliation_group_id, rule_id, rule_version,
			calculated_variance_cents, tolerance_cents, exceeds_tolerance, calculation_formula, severity, status)
		 VALUES ($1, $2, 'gl_reconciliation', 'abc123', 500, 1, true, 'v = a - b', 'medium', 'open')
		 RETURNING id::text`, env.bookA, env.groupA)
	env.reportA = mustQueryRow(t, pool,
		`INSERT INTO audit_reports (client_book_id, period_start, period_end, generated_by, finding_ids)
		 VALUES ($1, '2026-01-01', '2026-01-31', $2, ARRAY[$3::uuid])
		 RETURNING id::text`, env.bookA, env.adminA, env.findingA)

	return env
}

// seedRotateKeys creates one active data encryption key for the firm.
func seedRotateKeys(t *testing.T, pool *pgxpool.Pool, firmID string) {
	t.Helper()
	mustQueryRow(t, pool,
		`INSERT INTO data_encryption_keys (firm_id, key_ref) VALUES ($1, 'kms:seed/1') RETURNING id::text`, firmID)
}

func (e *securityEnv) token(t *testing.T, userID, firmID, role string) string {
	t.Helper()
	as := auth.NewService()
	pair, err := as.GenerateTokens(userID, firmID, role)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	return pair.AccessToken
}

func (e *securityEnv) tamperedToken(t *testing.T, userID, firmID, role string) string {
	t.Helper()
	ok := e.token(t, userID, firmID, role)
	// Flip one char in the payload segment; signature becomes invalid.
	parts := strings.Split(ok, ".")
	if len(parts) != 3 {
		t.Fatal("unexpected JWT shape")
	}
	payload := []byte(parts[1])
	if payload[0] == 'A' {
		payload[0] = 'B'
	} else {
		payload[0] = 'A'
	}
	return parts[0] + "." + string(payload) + "." + parts[2]
}

// newRouter builds the production middleware chain (auth + RLS) with a minimal
// set of routes covering every cross-tenant access path.
func (e *securityEnv) newRouter(t *testing.T) http.Handler {
	t.Helper()
	as := auth.NewService()

	tenantSvc := tenant.NewService()
	tenantSvc.SetDB(e.pool)
	docSvc := documents.NewService()
	docSvc.SetDB(e.pool)
	findingSvc := findings.NewService()
	findingSvc.SetDB(e.pool)
	reviewSvc := review.NewService()
	reviewSvc.SetDB(e.pool)

	r := chi.NewRouter()
	r.Use(middleware.Authenticator(as))
	r.Use(middleware.RLSInjector(e.pool))

	r.Route("/v1/books", func(r chi.Router) {
		r.Get("/", tenantSvc.HandleListBooks)
		r.Get("/{bookId}", tenantSvc.HandleGetBook)
		r.Route("/{bookId}/documents", func(r chi.Router) {
			r.Get("/", docSvc.HandleList)
			r.Get("/{docId}", docSvc.HandleGet)
		})
		r.Get("/{bookId}/findings", findingSvc.HandleList)
		r.Get("/{bookId}/review-queue", reviewSvc.HandleList)
	})
	r.Post("/v1/entity-links/{linkId}/confirm", reviewSvc.HandleConfirm)
	r.Post("/v1/entity-links/{linkId}/reject", reviewSvc.HandleReject)
	r.Post("/v1/books/{bookId}/reports", findingSvc.HandleGenerateReport)
	r.Get("/v1/reports/{reportId}", findingSvc.HandleGetReport)
	r.Get("/v1/reports/{reportId}/citation/{findingId}", findingSvc.HandleGetCitation)
	return r
}

func (e *securityEnv) do(t *testing.T, router http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var payload struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode list body (%d): %v", rec.Code, err)
	}
	return payload.Items
}

// countAccessLog returns the number of access_log rows for a user+action.
func (e *securityEnv) countAccessLog(t *testing.T, userID, action string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM access_log WHERE user_id = $1 AND action = $2`,
		userID, action).Scan(&n); err != nil {
		t.Fatalf("failed to count access_log: %v", err)
	}
	return n
}

// ---- 1. Staff assigned to Book A cannot access Book B's document by ID ----

func TestSecurity_CrossBookDocumentAccessReturns404(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)
	token := env.token(t, env.staffA, env.firmA, "staff")

	// docB exists but belongs to firm B — must be invisible.
	rec := env.do(t, router, "GET", "/v1/books/"+env.bookB+"/documents/"+env.docB, token, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-book document, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem") {
		t.Errorf("expected RFC 7807 problem+json, got %q", ct)
	}
}

// ---- 2. Staff from Firm X cannot access Firm Y's book ----

func TestSecurity_CrossFirmBookAccessReturns404(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)
	token := env.token(t, env.staffA, env.firmA, "staff")

	rec := env.do(t, router, "GET", "/v1/books/"+env.bookB, token, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-firm book, got %d", rec.Code)
	}
}

// ---- 3. Staff cannot list books they're not assigned to (RLS filters rows) ----

func TestSecurity_StaffListBooksIsFiltered(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)
	token := env.token(t, env.staffA, env.firmA, "staff")

	rec := env.do(t, router, "GET", "/v1/books/", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var books []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
		t.Fatalf("failed to decode books: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("staff assigned to one book should see exactly 1, got %d", len(books))
	}
	if books[0]["id"] != env.bookA {
		t.Errorf("expected book %s, got %s", env.bookA, books[0]["id"])
	}
}

// ---- 4. firm_admin sees ALL books in their firm; staff sees only assigned ----

func TestSecurity_AdminVsStaffBookVisibility(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	t.Run("firm_admin sees all firm books", func(t *testing.T) {
		rec := env.do(t, router, "GET", "/v1/books/", env.token(t, env.adminA, env.firmA, "firm_admin"), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var books []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatalf("failed to decode books: %v", err)
		}
		if len(books) != 2 {
			t.Fatalf("firm_admin should see both firm-A books, got %d", len(books))
		}
	})

	t.Run("staff sees only assigned books", func(t *testing.T) {
		rec := env.do(t, router, "GET", "/v1/books/", env.token(t, env.staffA, env.firmA, "staff"), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var books []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatalf("failed to decode books: %v", err)
		}
		if len(books) != 1 || books[0]["id"] != env.bookA {
			t.Fatalf("staff should see only their book, got %d entries", len(books))
		}
	})
}

// ---- 5. Unauthenticated request -> 401 ----

func TestSecurity_UnauthenticatedReturns401(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	for _, path := range []string{"/v1/books/", "/v1/books/" + env.bookA, "/v1/reports/" + env.reportA} {
		rec := env.do(t, router, "GET", path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for %s, got %d", path, rec.Code)
		}
	}
}

// ---- 6. Crafted session var injection in a book ID -> rejected/404 ----

func TestSecurity_SessionVarInjectionRejected(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)
	token := env.token(t, env.staffA, env.firmA, "staff")

	payloads := []string{
		"'; DROP TABLE access_log; --",
		"' OR '1'='1",
		"1; SELECT pg_sleep(10); --",
		"' UNION SELECT id FROM firms; --",
	}
	for _, payload := range payloads {
		rec := env.do(t, router, "GET", "/v1/books/"+payload, token, "")
		// Any non-2xx is acceptable — the requirement is the injection never
		// executes and never 200s with data.
		if rec.Code == http.StatusOK {
			t.Errorf("injection payload %q returned 200 with data", payload)
		}
	}

	// The drop target must still exist (injection did not run).
	var exists bool
	if err := env.pool.QueryRow(context.Background(),
		"SELECT to_regclass('public.access_log') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("verification query failed: %v", err)
	}
	if !exists {
		t.Fatal("access_log was dropped — SQL injection succeeded")
	}
}

// ---- 7. JWT tampering -> 401 (signature validation) ----

func TestSecurity_JWTTamperingRejected(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	bad := env.tamperedToken(t, env.adminA, env.firmA, "firm_admin")
	rec := env.do(t, router, "GET", "/v1/books/", bad, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered JWT, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem") {
		t.Errorf("expected RFC 7807 problem+json, got %q", ct)
	}
}

// ---- 8. User with no book assignments sees empty list ----

func TestSecurity_NoAssignmentsSeesEmptyList(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	// Create a staff user in firm A with no book assignments.
	noBooks := mustQueryRow(t, env.pool,
		`INSERT INTO users (firm_id, email, password_hash, role, email_verified)
		 VALUES ($1, 'no-books@test.local', 'unused', 'staff', true) RETURNING id::text`, env.firmA)

	rec := env.do(t, router, "GET", "/v1/books/", env.token(t, noBooks, env.firmA, "staff"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("expected empty list, got %q", body)
	}

	// Direct document access must also 404.
	rec = env.do(t, router, "GET", "/v1/books/"+env.bookA+"/documents/"+env.docA, env.token(t, noBooks, env.firmA, "staff"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for document outside assignment, got %d", rec.Code)
	}
}

// ---- 9. Finding/document/report from another book cannot be fetched by ID ----

func TestSecurity_CrossBookResourceByIDReturns404(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	// Seed a finding, group and report inside firm B's book so there is
	// real cross-tenant data to try to reach.
	groupB := mustQueryRow(t, env.pool,
		`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status)
		 VALUES ($1, 0.9, 'needs_review') RETURNING id::text`, env.bookB)
	findingB := mustQueryRow(t, env.pool,
		`INSERT INTO audit_findings (client_book_id, reconciliation_group_id, rule_id, rule_version,
			calculated_variance_cents, tolerance_cents, exceeds_tolerance, calculation_formula, severity, status)
		 VALUES ($1, $2, 'gl_reconciliation', 'abc123', 500, 1, true, 'v = a - b', 'medium', 'open')
		 RETURNING id::text`, env.bookB, groupB)
	reportB := mustQueryRow(t, env.pool,
		`INSERT INTO audit_reports (client_book_id, period_start, period_end, generated_by, finding_ids)
		 VALUES ($1, '2026-01-01', '2026-01-31', $2, ARRAY[$3::uuid])
		 RETURNING id::text`, env.bookB, env.adminB, findingB)

	token := env.token(t, env.staffA, env.firmA, "staff")
	tokenA := env.token(t, env.adminA, env.firmA, "firm_admin")

	cases := []struct {
		name   string
		method string
		path   string
		token  string
	}{
		{"finding by id from firm B book", "GET", "/v1/reports/" + reportB + "/citation/" + findingB, token},
		{"report by id from firm B book", "GET", "/v1/reports/" + reportB, token},
		{"document by id from firm B book", "GET", "/v1/books/" + env.bookB + "/documents/" + env.docB, token},
		{"firm B group confirm as firm A staff", "POST", "/v1/entity-links/" + groupB + "/confirm", token},
		{"firm B group reject as firm A staff", "POST", "/v1/entity-links/" + groupB + "/reject", token},
		{"firm B report by id as firm A admin", "GET", "/v1/reports/" + reportB, tokenA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, router, tc.method, tc.path, tc.token, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("expected 404 (no existence leak), got %d", rec.Code)
			}
		})
	}
}

// ---- 10. Access logging: sensitive routes write access_log rows ----

func TestSecurity_AccessLogWrittenOnSensitiveActions(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)
	token := env.token(t, env.staffA, env.firmA, "staff")
	tokenA := env.token(t, env.adminA, env.firmA, "firm_admin")

	// GET document metadata.
	rec := env.do(t, router, "GET", "/v1/books/"+env.bookA+"/documents/"+env.docA, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 fetching own document, got %d", rec.Code)
	}
	// Confirm an owned group.
	rec = env.do(t, router, "POST", "/v1/entity-links/"+env.groupA+"/confirm", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming own link, got %d", rec.Code)
	}
	// Reject it too (status resets to needs_review is not enforced; fine).
	rec = env.do(t, router, "POST", "/v1/entity-links/"+env.groupA+"/reject", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 rejecting own link, got %d", rec.Code)
	}
	// Generate a report.
	rec = env.do(t, router, "POST", "/v1/books/"+env.bookA+"/reports",
		tokenA, `{"period_start":"2026-01-01","period_end":"2026-01-31"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 generating report, got %d", rec.Code)
	}
	var report struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil || report.ID == "" {
		t.Fatalf("failed to decode generated report: %v (%q)", err, rec.Body.String())
	}
	// Download the report.
	rec = env.do(t, router, "GET", "/v1/reports/"+report.ID, tokenA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 downloading report, got %d", rec.Code)
	}

	for _, tc := range []struct {
		action string
		min    int
	}{
		{"view_document", 1},
		{"confirm_link", 1},
		{"reject_link", 1},
		{"generate_report", 1},
		{"download_report", 1},
	} {
		if got := env.countAccessLog(t, env.staffA, tc.action); tc.action == "generate_report" || tc.action == "download_report" {
			// Those actions are performed by adminA.
			if got := env.countAccessLog(t, env.adminA, tc.action); got < tc.min {
				t.Errorf("expected >= %d access_log rows for %s by adminA, got %d", tc.min, tc.action, got)
			}
		} else if got < tc.min {
			t.Errorf("expected >= %d access_log rows for %s, got %d", tc.min, tc.action, got)
		}
	}
}

// ---- 11. Key rotation endpoint (Task 4) ----

func TestSecurity_AdminRotateKeys(t *testing.T) {
	env := setupEnv(t)
	seedRotateKeys(t, env.pool, env.firmA)

	adminToken := env.token(t, env.adminA, env.firmA, "firm_admin")
	staffToken := env.token(t, env.staffA, env.firmA, "staff")

	// Build the production chain order: Authenticator -> RLSInjector -> RequireRole.
	tenantSvc := tenant.NewService()
	tenantSvc.SetDB(env.pool)
	router3 := chi.NewRouter()
	router3.Use(middleware.Authenticator(auth.NewService()))
	router3.Use(middleware.RLSInjector(env.pool))
	router3.Route("/v1/admin", func(r chi.Router) {
		r.Use(middleware.RequireRole("firm_admin"))
		r.Post("/rotate-keys", tenantSvc.HandleRotateKeys)
	})

	rec := env.do(t, router3, "POST", "/v1/admin/rotate-keys", adminToken, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 rotating keys, got %d (body %q)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		KeyRef string `json:"key_ref"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode rotate response: %v", err)
	}
	if created.Status != "active" {
		t.Errorf("expected new key active, got %q", created.Status)
	}
	if !strings.HasPrefix(created.KeyRef, "kms:") {
		t.Errorf("key_ref should be a KMS reference, got %q", created.KeyRef)
	}

	// Exactly one active key for firm A; the seed key moved to rotating.
	var active int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM data_encryption_keys WHERE firm_id = $1 AND status = 'active'`,
		env.firmA).Scan(&active); err != nil {
		t.Fatalf("failed to count active keys: %v", err)
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active key after rotation, got %d", active)
	}

	// Staff cannot rotate keys (role check).
	rec = env.do(t, router3, "POST", "/v1/admin/rotate-keys", staffToken, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for staff key rotation, got %d", rec.Code)
	}
}
