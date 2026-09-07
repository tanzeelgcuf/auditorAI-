// Package middleware_test — Phase 5 multi-tenancy security hardening tests.
//
// These tests run against a REAL Postgres with the infra/init.sql schema and
// RLS policies applied. They skip cleanly when no test database is available.
//
// TWO DSNs, and the split is the whole point of this file:
//
//	DATABASE_URL_TEST        the RLS-ENFORCED role (auditor_app). This is the pool
//	                         handed to RLSInjector and to the services under test,
//	                         so every assertion below exercises the same
//	                         enforcement path production uses.
//	DATABASE_URL_TEST_OWNER  the TABLE OWNER (in CI: the `auditor` bootstrap
//	                         superuser). Used ONLY for fixture setup and for
//	                         cross-tenant verification reads.
//
// DATABASE_URL_TEST_OWNER must be the owner, not auditor_sys: init.sql:766 grants
// both roles SELECT/INSERT/UPDATE/DELETE and nothing more, so auditor_sys — which
// does have BYPASSRLS — would still fail setupEnv's TRUNCATE with "permission
// denied". BYPASSRLS alone is not sufficient here; ownership is.
//
// Setup cannot run on the app role, and that is not a workaround — it is the
// evidence that RLS is real. auditor_app is granted SELECT/INSERT/UPDATE/DELETE
// and deliberately NOT TRUNCATE, and `INSERT INTO firms` cannot satisfy a policy
// predicate that references the firm row being created. If this file ever passes
// with both variables pointing at the same role, the suite is not testing
// isolation.
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

// securityEnv holds the two DB pools and the ids of all seeded entities.
type securityEnv struct {
	// pool is the RLS-ENFORCED pool. Everything under test runs on it.
	pool *pgxpool.Pool
	// setupPool bypasses RLS. Fixtures and cross-tenant verification reads only —
	// never wire it into a handler or a middleware, or the test proves nothing.
	setupPool *pgxpool.Pool

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

// setupDSN returns the BYPASSRLS DSN used for fixtures. It does NOT fall back to
// testDSN(): a silent fallback would let the suite go green against a single
// over-privileged role, which is the exact failure this file exists to detect.
func setupDSN() string {
	return os.Getenv("DATABASE_URL_TEST_OWNER")
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

// requireSetupDB returns the BYPASSRLS pool used for fixtures, and FAILS rather
// than skips when it is misconfigured — a skipped security suite reads as green.
func requireSetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := setupDSN()
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST_OWNER not set; skipping multi-tenancy security test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no setup database available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("setup database not reachable: %v", err)
	}
	return pool
}

// assertPoolsDiffer proves the two DSNs resolve to different enforcement
// postures before any assertion is made. Without this, pointing both variables at
// the same superuser turns every test below into a tautology — which is precisely
// how this suite passed for as long as it did.
func assertPoolsDiffer(t *testing.T, app, setup *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var appRLS, setupRLS bool
	var appRole, setupRole string
	if err := app.QueryRow(ctx,
		`SELECT current_user, row_security_active('client_books'::regclass)`).
		Scan(&appRole, &appRLS); err != nil {
		t.Fatalf("app pool posture query failed: %v", err)
	}
	if err := setup.QueryRow(ctx,
		`SELECT current_user, row_security_active('client_books'::regclass)`).
		Scan(&setupRole, &setupRLS); err != nil {
		t.Fatalf("setup pool posture query failed: %v", err)
	}
	if !appRLS {
		t.Fatalf("DATABASE_URL_TEST role %q is NOT subject to row security — "+
			"every isolation assertion in this file would pass vacuously. "+
			"Point it at auditor_app", appRole)
	}
	if setupRLS {
		t.Fatalf("DATABASE_URL_TEST_OWNER role %q IS subject to row security, so "+
			"it cannot seed fixtures. Point it at the table owner (the `auditor` "+
			"bootstrap role) — not auditor_sys, which bypasses RLS but was never "+
			"granted TRUNCATE", setupRole)
	}
	t.Logf("posture ok: app=%s row_security_active=true, setup=%s row_security_active=false",
		appRole, setupRole)
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
//
// All seeding runs on setupPool (BYPASSRLS). The returned env.pool is the
// RLS-enforced pool that the middleware and services under test are wired to.
func setupEnv(t *testing.T) *securityEnv {
	t.Helper()
	appPool := requireDB(t)
	t.Cleanup(appPool.Close)
	pool := requireSetupDB(t)
	t.Cleanup(pool.Close)
	assertPoolsDiffer(t, appPool, pool)
	ctx := context.Background()

	// Truncate everything with tenant scope (CASCADE reaches the rest).
	// TRUNCATE is deliberately NOT granted to auditor_app, so this only works on
	// setupPool — see the package comment.
	if _, err := pool.Exec(ctx,
		`TRUNCATE firms, users, client_books, user_book_assignments, source_documents,
			extracted_entities, reconciliation_groups, audit_findings, audit_reports,
			access_log, config_change_log, data_encryption_keys CASCADE`); err != nil {
		t.Fatalf("failed to truncate test tables: %v", err)
	}

	env := &securityEnv{pool: appPool, setupPool: pool}

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
		r.Patch("/{bookId}/settings", tenantSvc.HandleUpdateBookSettings)
		// Mirrors main.go exactly, including the per-route RequireRole. Mounting
		// these WITHOUT the gate is what production did until 2026-09-06, so a test
		// router that omits it would be testing a system nobody ships.
		r.With(middleware.RequireRole("firm_admin")).Post("/{bookId}/staff", tenantSvc.HandleAssignStaff)
		r.With(middleware.RequireRole("firm_admin")).Delete("/{bookId}/staff/{userId}", tenantSvc.HandleRemoveStaff)
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
	if err := e.setupPool.QueryRow(context.Background(),
		`SELECT count(*) FROM access_log WHERE user_id = $1 AND action = $2`,
		userID, action).Scan(&n); err != nil {
		t.Fatalf("failed to count access_log: %v", err)
	}
	return n
}

// ---- helpers for the two audit-content cases (source_ip, config_change_log) ----

// withClientIP wraps an already-built chain in the production RealIP -> SourceIP
// pair, in that order. The order is the thing under test in
// TestSecurity_AccessLogSourceIPMatchesTheLimiterResolution: SourceIP reads
// peerIP(r), which is RealIP's already-rewritten RemoteAddr, so mounted above
// RealIP it records the PROXY's address on every request behind a trusted proxy —
// silently, with no error and no log line.
func (e *securityEnv) withClientIP(t *testing.T, inner http.Handler, cidrs string) (http.Handler, middleware.TrustedProxies) {
	t.Helper()
	tp, bad := middleware.ParseTrustedProxies(cidrs)
	if len(bad) != 0 {
		t.Fatalf("test wrote an unparseable TRUSTED_PROXY_CIDRS spec %q: %v", cidrs, bad)
	}
	return middleware.RealIP(tp)(middleware.SourceIP(inner)), tp
}

// doFrom is do() with control over the transport-level facts an audit row is
// supposed to capture: who opened the connection, and what they claimed in
// forwarding headers. It returns the request it served so the caller can hand
// the identical request to middleware.ClientIP and compare the DB row against an
// independently computed value rather than a hardcoded string.
func (e *securityEnv) doFrom(t *testing.T, router http.Handler, method, path, token, body,
	remoteAddr string, headers map[string]string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// ClientIP must run against an untouched copy: RealIP mutates RemoteAddr in
	// place, so computing the expectation afterwards would compare the middleware
	// to itself.
	expectSrc := req.Clone(req.Context())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec, expectSrc
}

// lastSourceIP reads access_log.source_ip for a user+action as text, or nil when
// the column is NULL. A NULL here is not a neutral outcome: it means the request
// never passed middleware.SourceIP, so the row says the action came from nowhere.
func (e *securityEnv) lastSourceIP(t *testing.T, userID, action string) *string {
	t.Helper()
	var ip *string
	if err := e.setupPool.QueryRow(context.Background(),
		`SELECT source_ip::text FROM access_log
		  WHERE user_id = $1 AND action = $2 ORDER BY id DESC LIMIT 1`,
		userID, action).Scan(&ip); err != nil {
		t.Fatalf("failed to read access_log.source_ip for %s/%s: %v", userID, action, err)
	}
	return ip
}

type configChangeRow struct {
	Field    string
	OldValue *string
	NewValue *string
	SourceIP *string
}

// configChanges reads every config_change_log row for a book, oldest first.
func (e *securityEnv) configChanges(t *testing.T, bookID string) []configChangeRow {
	t.Helper()
	rows, err := e.setupPool.Query(context.Background(),
		`SELECT field_name, old_value, new_value, source_ip::text
		   FROM config_change_log WHERE client_book_id = $1 ORDER BY id`, bookID)
	if err != nil {
		t.Fatalf("failed to read config_change_log: %v", err)
	}
	defer rows.Close()
	var out []configChangeRow
	for rows.Next() {
		var r configChangeRow
		if err := rows.Scan(&r.Field, &r.OldValue, &r.NewValue, &r.SourceIP); err != nil {
			t.Fatalf("failed to scan config_change_log row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("config_change_log iteration failed: %v", err)
	}
	return out
}

// assignmentExists reads user_book_assignments through the OWNER pool, which is
// initdb's bootstrap role and therefore a SUPERUSER (init.sql:816), so it sees the
// row regardless of FORCE ROW LEVEL SECURITY. That is the whole point: the
// question "did the write land?" cannot be asked through the same policies the
// write was supposed to be stopped by.
func (e *securityEnv) assignmentExists(t *testing.T, userID, bookID string) bool {
	t.Helper()
	var n int
	if err := e.setupPool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_book_assignments
		  WHERE user_id::text = $1 AND client_book_id::text = $2`,
		userID, bookID).Scan(&n); err != nil {
		t.Fatalf("failed to read user_book_assignments: %v", err)
	}
	return n > 0
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
	if err := env.setupPool.QueryRow(context.Background(),
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
	noBooks := mustQueryRow(t, env.setupPool,
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
	groupB := mustQueryRow(t, env.setupPool,
		`INSERT INTO reconciliation_groups (client_book_id, link_confidence, status)
		 VALUES ($1, 0.9, 'needs_review') RETURNING id::text`, env.bookB)
	findingB := mustQueryRow(t, env.setupPool,
		`INSERT INTO audit_findings (client_book_id, reconciliation_group_id, rule_id, rule_version,
			calculated_variance_cents, tolerance_cents, exceeds_tolerance, calculation_formula, severity, status)
		 VALUES ($1, $2, 'gl_reconciliation', 'abc123', 500, 1, true, 'v = a - b', 'medium', 'open')
		 RETURNING id::text`, env.bookB, groupB)
	reportB := mustQueryRow(t, env.setupPool,
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
	seedRotateKeys(t, env.setupPool, env.firmA)

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
	if err := env.setupPool.QueryRow(context.Background(),
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

// ---- 12. access_log.source_ip agrees with the rate limiter about who called ----

// The existing access-log test counts rows by user+action and never looks at
// source_ip, so a row recording the wrong caller — or no caller — passed it. This
// asserts the column against middleware.ClientIP computed independently from the
// same request, which is the same resolution the per-IP rate limiter buckets on.
// One resolution point is the whole design (clientip.go): if the audit trail and
// the limiter can disagree about who called, neither number means anything.
func TestSecurity_AccessLogSourceIPMatchesTheLimiterResolution(t *testing.T) {
	env := setupEnv(t)
	token := env.token(t, env.staffA, env.firmA, "staff")

	t.Run("behind a trusted proxy the row records the client, not the proxy", func(t *testing.T) {
		chain, tp := env.withClientIP(t, env.newRouter(t), "192.0.2.0/24")
		rec, served := env.doFrom(t, chain, "GET",
			"/v1/books/"+env.bookA+"/documents/"+env.docA, token, "",
			"192.0.2.7:41000", map[string]string{"X-Forwarded-For": "203.0.113.9"})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 fetching own document, got %d (%q)", rec.Code, rec.Body.String())
		}
		want := middleware.ClientIP(served, tp)
		if want != "203.0.113.9" {
			t.Fatalf("test setup wrong: ClientIP resolved %q, expected the XFF client "+
				"203.0.113.9 for a peer inside 192.0.2.0/24", want)
		}
		got := env.lastSourceIP(t, env.staffA, "view_document")
		if got == nil {
			t.Fatal("access_log.source_ip is NULL — the row says this action came from " +
				"nowhere. Either SourceIP is not mounted or it ran above RealIP")
		}
		if *got == "192.0.2.7" {
			t.Fatalf("access_log.source_ip = %q, the PROXY's address. This is the "+
				"RealIP/SourceIP ordering bug: SourceIP reads the rewritten "+
				"RemoteAddr, so mounted above RealIP it records the hop", *got)
		}
		if *got != want {
			t.Errorf("access_log.source_ip = %q, but ClientIP resolved %q for the same "+
				"request — the audit trail and the rate limiter disagree", *got, want)
		}
	})

	t.Run("an untrusted peer cannot forge it with a header", func(t *testing.T) {
		// Same spoof attempt, but the peer is outside the trusted set. Every
		// forwarding header must be ignored and the peer recorded.
		chain, tp := env.withClientIP(t, env.newRouter(t), "192.0.2.0/24")
		rec, served := env.doFrom(t, chain, "GET",
			"/v1/books/"+env.bookA+"/documents/"+env.docA, token, "",
			"198.51.100.5:52000", map[string]string{
				"X-Forwarded-For": "203.0.113.1",
				"X-Real-IP":       "203.0.113.2",
			})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 fetching own document, got %d", rec.Code)
		}
		want := middleware.ClientIP(served, tp)
		if want != "198.51.100.5" {
			t.Fatalf("test setup wrong: ClientIP resolved %q for an UNTRUSTED peer; "+
				"headers must be ignored and the peer returned", want)
		}
		got := env.lastSourceIP(t, env.staffA, "view_document")
		if got == nil {
			t.Fatal("access_log.source_ip is NULL for a direct request")
		}
		if *got == "203.0.113.1" || *got == "203.0.113.2" {
			t.Fatalf("access_log.source_ip = %q — a header-supplied value from an "+
				"untrusted peer. The audit trail is forgeable by the party it records", *got)
		}
		if *got != want {
			t.Errorf("access_log.source_ip = %q, want the peer %q", *got, want)
		}
	})
}

// ---- 13. A config change lands in config_change_log, and only the real one ----

// This is the case that settles two things previously reasoned-only: that
// LogConfigChange's write reaches the table at all (it runs on the request's
// RLS-primed connection; on the raw pool the policy predicate raises on the unset
// app.assigned_books GUC and the error degrades to a slog.Warn, so
// GET /config-history would return empty forever), and that a PATCH records
// exactly the fields it changed.
//
// The count assertion is the discriminating one. Before 2026-09-06 this returned
// FIVE rows for a one-field PATCH: auditConfigChange took `v interface{}` and
// every call site passes a typed pointer out of the decoded body, so `v == nil`
// was never true — a nil *string inside an interface is not equal to nil. Four
// rows claimed untouched fields had changed with new_value "<nil>", and the real
// row recorded a hex pointer address instead of the value.
func TestSecurity_ConfigChangeAuditRecordsExactlyWhatChanged(t *testing.T) {
	env := setupEnv(t)
	if pre := env.configChanges(t, env.bookA); len(pre) != 0 {
		t.Fatalf("expected config_change_log empty after setup, got %d rows", len(pre))
	}
	chain, tp := env.withClientIP(t, env.newRouter(t), "")
	token := env.token(t, env.staffA, env.firmA, "staff")

	rec, served := env.doFrom(t, chain, "PATCH", "/v1/books/"+env.bookA+"/settings",
		token, `{"auto_link_confidence_threshold":0.97}`, "198.51.100.9:33000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 patching own book settings, got %d (%q)", rec.Code, rec.Body.String())
	}

	rows := env.configChanges(t, env.bookA)
	if len(rows) == 0 {
		t.Fatal("config_change_log is EMPTY after a successful settings PATCH. The " +
			"write is swallowed into a slog.Warn, so this is silent: check that " +
			"LogConfigChange uses middleware.DB(ctx, db) and that the route is inside " +
			"the RLSInjector group")
	}
	if len(rows) != 1 {
		var got []string
		for _, r := range rows {
			nv := "NULL"
			if r.NewValue != nil {
				nv = *r.NewValue
			}
			got = append(got, r.Field+"="+nv)
		}
		t.Fatalf("one field was PATCHed but config_change_log has %d rows: %s. Fields "+
			"absent from the request body must not be audited as changes",
			len(rows), strings.Join(got, ", "))
	}

	row := rows[0]
	if row.Field != "auto_link_confidence_threshold" {
		t.Errorf("field_name = %q, want auto_link_confidence_threshold", row.Field)
	}
	if row.NewValue == nil {
		t.Fatal("new_value is NULL for a field that was explicitly set to 0.97")
	}
	switch {
	case *row.NewValue == "<nil>":
		t.Errorf(`new_value = "<nil>" — a nil typed pointer formatted by fmt.Sprint ` +
			`instead of being recognised as absent`)
	case strings.HasPrefix(*row.NewValue, "0x"):
		t.Errorf("new_value = %q — that is a pointer address, not the value the user "+
			"set. strVal has no pointer case in its type switch", *row.NewValue)
	case *row.NewValue != "0.97":
		t.Errorf("new_value = %q, want %q", *row.NewValue, "0.97")
	}
	if row.OldValue != nil {
		t.Logf("old_value = %q (capture is documented as deferred; noted, not failed)", *row.OldValue)
	}
	if row.SourceIP == nil {
		t.Error("config_change_log.source_ip is NULL — the row does not say where the " +
			"change came from")
	} else if want := middleware.ClientIP(served, tp); *row.SourceIP != want {
		t.Errorf("config_change_log.source_ip = %q, but ClientIP resolved %q for the "+
			"same request", *row.SourceIP, want)
	}

	// The value must also have actually landed on the book — an audit row for a
	// change that did not happen is its own defect. The equality is evaluated by
	// Postgres against the NUMERIC(4,3) column rather than scanned into a Go
	// float64 and compared there: the stored value is exact decimal, and pulling it
	// through binary floating point to check it is the habit this project bans for
	// money and has no reason to practise here either.
	var landed bool
	var asText string
	if err := env.setupPool.QueryRow(context.Background(),
		`SELECT auto_link_confidence_threshold = 0.97,
		        auto_link_confidence_threshold::text
		   FROM client_books WHERE id = $1`,
		env.bookA).Scan(&landed, &asText); err != nil {
		t.Fatalf("failed to read back the patched threshold: %v", err)
	}
	if !landed {
		t.Errorf("client_books.auto_link_confidence_threshold = %s, want 0.97 — the "+
			"audit row records a change that did not reach the table", asText)
	}
}

// ---- 14. Staff cannot assign themselves to a book (privilege escalation) ----

// TestSecurity_StaffCannotSelfAssignToUnassignedBook is the regression test for the
// escalation found 2026-09-06 while writing case 13.
//
// What was wrong: main.go mounted POST /v1/books/{bookId}/staff and
// DELETE /v1/books/{bookId}/staff/{userId} in the general protected group, with no
// RequireRole anywhere near them — the only RequireRole in the file guarded
// /v1/admin. Both handlers nevertheless carried comments asserting the opposite
// ("Only firm_admin can assign staff — enforced by RequireRole middleware at
// /v1/admin"). The comments described an intention the router never applied.
//
// Why it mattered more than a missing role check usually does: of every table in
// this schema, user_book_assignments is the one whose policy gates on FIRM, not on
// app.assigned_books — assignments_own_firm_only, init.sql:606. So book-level
// containment for that table lived entirely in Go, and the handler checked neither
// the caller's role nor bookId against the caller's own assignments. A staff JWT
// could POST its own user_id to any book in its firm, and RLSInjector rebuilds
// app.assigned_books from that table on the very next request. One request, and the
// second level of the two-level RLS model is gone for that book: documents,
// entities, groups, findings, reports, config_change_log, access_log, all readable.
//
// The write-side assertions matter as much as the status codes. A 403 with the row
// present would mean the gate returned early on the response while the handler ran
// anyway; only assignmentExists can tell those apart, and it reads through the
// superuser pool so FORCE ROW LEVEL SECURITY cannot hide the answer.
//
// Both halves are here on purpose. The negative subtests fail against the pre-fix
// router (they were the exploit). The positive subtests fail if someone "fixes" a
// future problem by making the route admin-only-and-broken, which is the shape this
// project has watched a guard rot back into.
func TestSecurity_StaffCannotSelfAssignToUnassignedBook(t *testing.T) {
	env := setupEnv(t)
	router := env.newRouter(t)

	staffToken := env.token(t, env.staffA, env.firmA, "staff")
	adminToken := env.token(t, env.adminA, env.firmA, "firm_admin")

	// Precondition, asserted rather than assumed: staffA is seeded into bookA only,
	// so bookA2 is a same-firm book it has no claim on. If the fixture ever changes
	// to pre-assign it, every subtest below silently stops testing anything.
	if !env.assignmentExists(t, env.staffA, env.bookA) {
		t.Fatal("fixture broken: staffA should be assigned to bookA")
	}
	if env.assignmentExists(t, env.staffA, env.bookA2) {
		t.Fatal("fixture broken: staffA must NOT start out assigned to bookA2, or this " +
			"test cannot observe the escalation")
	}

	t.Run("staff self-assigning to a same-firm book it is not on", func(t *testing.T) {
		rec := env.do(t, router, "POST", "/v1/books/"+env.bookA2+"/staff", staffToken,
			`{"user_id":"`+env.staffA+`"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST /v1/books/%s/staff as staff = %d, want 403. Body: %s",
				env.bookA2, rec.Code, rec.Body.String())
		}
		if env.assignmentExists(t, env.staffA, env.bookA2) {
			t.Fatalf("user_book_assignments now contains (staffA, bookA2) — staff granted "+
				"ITSELF access to a book it was never assigned to. RLSInjector will put "+
				"bookA2 into app.assigned_books on the next request, so every row in that "+
				"book is now readable by this user. Response was %d.", rec.Code)
		}
	})

	t.Run("staff assigning a third party to a book it is not on", func(t *testing.T) {
		// Same defect, without the self-service framing — worth its own case because a
		// role check on "can only add yourself" would pass the subtest above.
		rec := env.do(t, router, "POST", "/v1/books/"+env.bookA2+"/staff", staffToken,
			`{"user_id":"`+env.adminA+`"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("staff adding another user to bookA2 = %d, want 403. Body: %s",
				rec.Code, rec.Body.String())
		}
		if env.assignmentExists(t, env.adminA, env.bookA2) {
			t.Error("user_book_assignments now contains (adminA, bookA2) — a staff user " +
				"edited the firm's book access map")
		}
	})

	t.Run("staff cannot unassign the firm admin", func(t *testing.T) {
		// The mirror image of the escalation: DELETE was mounted in the same ungated
		// group, so any staff user could revoke the admin's access to any book. That is
		// a denial of service against the only role that can undo it.
		rec := env.do(t, router, "DELETE",
			"/v1/books/"+env.bookA+"/staff/"+env.adminA, staffToken, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("DELETE staff as staff = %d, want 403. Body: %s", rec.Code, rec.Body.String())
		}
		if !env.assignmentExists(t, env.adminA, env.bookA) {
			t.Fatal("(adminA, bookA) was DELETED by a staff-role caller — staff can strip " +
				"the firm admin's access to a book")
		}
	})

	t.Run("firm_admin can still assign within its own firm", func(t *testing.T) {
		rec := env.do(t, router, "POST", "/v1/books/"+env.bookA2+"/staff", adminToken,
			`{"user_id":"`+env.staffA+`"}`)
		switch rec.Code {
		case http.StatusOK:
		case http.StatusBadRequest:
			// Distinct message on purpose: 400 here is "bookId is required", which means
			// r.PathValue("bookId") came back empty. That is not this fix — it is the open
			// chi question (46 r.PathValue sites against go-chi/chi/v5 v5.1.0, which only
			// began populating r.PathValue in v5.2.0). Fail loudly and name it rather than
			// letting it read as a broken authorization change.
			t.Fatalf("POST as firm_admin = 400 %q. If that is \"bookId is required\", "+
				"r.PathValue returned empty and the chi pin is the cause, not the role "+
				"gate — see the chi v5.1.0/v5.2.0 note in CLAUDE.md", rec.Body.String())
		default:
			t.Fatalf("POST as firm_admin = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		if !env.assignmentExists(t, env.staffA, env.bookA2) {
			t.Error("firm_admin got 200 but no row landed in user_book_assignments — the " +
				"gate now blocks the legitimate path too")
		}
	})

	t.Run("firm_admin cannot reach another firm's book", func(t *testing.T) {
		// 404, not 403: the role is sufficient, the book is invisible. The INSERT selects
		// its rows FROM client_books, which is firm-filtered on the primed connection, so
		// the row is unconstructible across firms even if the Go pre-check were deleted.
		rec := env.do(t, router, "POST", "/v1/books/"+env.bookB+"/staff", adminToken,
			`{"user_id":"`+env.staffA+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("firm A admin POSTing to firm B's book = %d, want 404. Body: %s",
				rec.Code, rec.Body.String())
		}
		if env.assignmentExists(t, env.staffA, env.bookB) {
			t.Fatal("a firm A user was assigned to a firm B book — cross-tenant write")
		}
	})

	t.Run("firm_admin cannot assign another firm's user", func(t *testing.T) {
		// The other axis: own book, foreign user. users is firm-scoped on the primed
		// connection too, so adminB is not selectable here.
		rec := env.do(t, router, "POST", "/v1/books/"+env.bookA+"/staff", adminToken,
			`{"user_id":"`+env.adminB+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("assigning a foreign firm's user = %d, want 404. Body: %s",
				rec.Code, rec.Body.String())
		}
		if env.assignmentExists(t, env.adminB, env.bookA) {
			t.Fatal("a firm B user now has an assignment to a firm A book — that user's " +
				"next request gets bookA in app.assigned_books")
		}
	})

	t.Run("another firm's admin cannot unassign into this firm", func(t *testing.T) {
		rec := env.do(t, router, "DELETE",
			"/v1/books/"+env.bookA+"/staff/"+env.adminA, env.token(t, env.adminB, env.firmB, "firm_admin"), "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("firm B admin deleting a firm A assignment = %d, want 404. Body: %s",
				rec.Code, rec.Body.String())
		}
		if !env.assignmentExists(t, env.adminA, env.bookA) {
			t.Fatal("(adminA, bookA) was deleted by a DIFFERENT firm's admin")
		}
	})
}
