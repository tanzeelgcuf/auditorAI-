package mcp

// HandleCreateEntityLink's status whitelist (no infrastructure needed).
//
// The endpoint took req.Status straight from the caller and put it in the
// INSERT. reconciliation_groups.status accepts five values, and three of them
// are not this endpoint's to record:
//
//	'confirmed' / 'rejected'  human dispositions, recorded through the review
//	                          endpoints after a person looks at the group.
//	'superseded'              set when a split/merge replaces a group.
//
// A group created as 'confirmed' is permanently immune to the deterministic
// downgrade: verify_worker.go guards its UPDATE with `AND status =
// 'auto_linked'` precisely so it never overwrites a human decision. So the group
// would carry an open exceeds_tolerance finding, read as human-confirmed, and
// never appear in the review queue (review.go selects on status) — with no human
// involved at any point.
//
// The fourth case, an unrecognised value, previously reached the INSERT and
// failed the CHECK constraint as a bare 500 "insert failed".
//
// WHY THESE TESTS NEED NO DATABASE, and why that is the assertion rather than a
// convenience: validation runs before the handler resolves a connection, and the
// resolution point now FAILS CLOSED with an observable response. A request built
// by httptest.NewRequest carries no connKey in its context, so
// middleware.GetConn returns nil and s.primed() answers 500 with detail
// "no db conn". An accepted request therefore stops at exactly that point and
// says so, which is what TestValidStatusReachesConnResolution asserts.
//
// Corrected 2026-09-05, and the reason it needed correcting is worth keeping:
// this block used to say "NewService() leaves db nil, so ... these requests
// would reach s.db.Acquire and panic on a nil pool", and the acceptance test
// recovered that panic. That premise died when the four GetConn-or-Acquire
// fallbacks in mcp.go were collapsed into s.primed() and the db field was
// deleted — there is no s.db and nothing panics. The test still PASSED, because
// it only checked "not 400", so nothing went red to announce that its stated
// mechanism no longer existed. Asserting the 500 and its detail replaces an
// absence-of-400 with a positive observation.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postLink(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := NewService()
	req := httptest.NewRequest(http.MethodPost, "/internal/mcp/create_entity_link",
		bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.HandleCreateEntityLink(rec, req)
	return rec
}

func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("response is not problem+json: %v (body %q)", err, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	if p.Status != rec.Code {
		t.Errorf("problem.status = %d, HTTP code = %d; they must agree", p.Status, rec.Code)
	}
	return p.Detail
}

// TestCreateEntityLinkRejectsHumanDispositions is the core of the fix: none of
// the three statuses this endpoint has no standing to record may be accepted.
func TestCreateEntityLinkRejectsHumanDispositions(t *testing.T) {
	for _, status := range []string{"confirmed", "rejected", "superseded"} {
		t.Run(status, func(t *testing.T) {
			rec := postLink(t, `{"bank_ids":["b1"],"gl_ids":["g1"],"confidence":1.0,"status":"`+status+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %q: got HTTP %d, want 400 — a group created as %q "+
					"is immune to verify_worker.go's `AND status = 'auto_linked'` downgrade",
					status, rec.Code, status)
			}
			detail := problemDetail(t, rec)
			if !strings.Contains(detail, "auto_linked") || !strings.Contains(detail, "needs_review") {
				t.Errorf("detail should name the two permitted values, got %q", detail)
			}
		})
	}
}

// TestCreateEntityLinkRejectsUnknownStatus — an unrecognised value used to reach
// the INSERT and surface as a 500 from the CHECK constraint.
func TestCreateEntityLinkRejectsUnknownStatus(t *testing.T) {
	for _, status := range []string{"linked", "AUTO_LINKED", "auto-linked", "pending", " auto_linked"} {
		rec := postLink(t, `{"bank_ids":["b1"],"gl_ids":["g1"],"status":"`+status+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %q: got HTTP %d, want 400", status, rec.Code)
		}
	}
}

// TestCreateEntityLinkRejectsRatherThanCoercing — silently rewriting a bad
// status to 'needs_review' would hide a wiring bug in the caller while throwing
// away its stated intent. The 400 is deliberate.
func TestCreateEntityLinkRejectsRatherThanCoercing(t *testing.T) {
	rec := postLink(t, `{"bank_ids":["b1"],"gl_ids":["g1"],"status":"confirmed"}`)
	if rec.Code == http.StatusCreated {
		t.Fatal("a 'confirmed' request was accepted; if it was coerced to " +
			"'needs_review' the caller's bug is now invisible")
	}
}

// TestCreateEntityLinkRequiresBankAndGL — doc 09: a group need not have all
// three legs (bank+GL only is valid for deposits and fees), but invoice-only is
// not a reconciliation of anything.
func TestCreateEntityLinkRequiresBankAndGL(t *testing.T) {
	cases := map[string]string{
		"no bank": `{"gl_ids":["g1"],"status":"needs_review"}`,
		"no gl":   `{"bank_ids":["b1"],"status":"needs_review"}`,
		"neither": `{"invoice_ids":["i1"],"status":"needs_review"}`,
		"empty":   `{}`,
	}
	for name, body := range cases {
		rec := postLink(t, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", name, rec.Code)
		}
	}
}

func TestCreateEntityLinkRejectsMalformedBody(t *testing.T) {
	rec := postLink(t, `{"bank_ids":`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: got HTTP %d, want 400", rec.Code)
	}
}

// TestValidStatusReachesConnResolution pins the whitelist from the other side:
// the two machine statuses, and the empty default, must NOT be rejected.
//
// The evidence of acceptance is a 500 with detail "no db conn" — s.primed()
// finding no RLS-primed connection in the request context. That is a positive
// observation, not merely the absence of a 400: it proves the body decoded, the
// bank+GL requirement passed, the status whitelist admitted the value, and the
// very next thing the handler reached was the single connection-resolution
// point. Any other code or detail means the request stopped somewhere else.
//
// Expect three "mcp: no RLS-primed connection" lines on stderr from primed()'s
// slog.Error while this test runs. They are the mechanism, not a failure.
func TestValidStatusReachesConnResolution(t *testing.T) {
	for _, status := range []string{"auto_linked", "needs_review", ""} {
		label := status
		if label == "" {
			label = "(unset, defaults to needs_review)"
		}
		rec := postLink(t, `{"bank_ids":["b1"],"gl_ids":["g1"],"status":"`+status+`"}`)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("status %s was rejected by the whitelist: %s",
				label, problemDetail(t, rec))
			continue
		}
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status %s: got HTTP %d, want 500 from s.primed() — an accepted "+
				"request must stop at the connection-resolution point, and this one "+
				"stopped somewhere else", label, rec.Code)
			continue
		}
		if detail := problemDetail(t, rec); detail != "no db conn" {
			t.Errorf("status %s: detail = %q, want \"no db conn\"; a different 500 "+
				"means the request failed past the resolution point, so this test is "+
				"no longer observing what it claims to", label, detail)
		}
	}
}
