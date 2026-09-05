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
// convenience: validation runs before the handler touches s.db. NewService()
// leaves db nil, so if the whitelist were removed these requests would reach
// s.db.Acquire and panic on a nil pool instead of returning 400. Reaching the DB
// at all is therefore observable here, which is what testValidStatusReachesDB
// relies on.

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

// TestValidStatusReachesDB pins the whitelist from the other side: the two
// machine statuses must NOT be rejected. With no pool wired, getting past
// validation means reaching s.db.Acquire on a nil pool, which panics — so the
// panic is the evidence of acceptance, and a 400 is the failure. Recovered here
// rather than in the handler, because in production the pool is never nil.
func TestValidStatusReachesDB(t *testing.T) {
	for _, status := range []string{"auto_linked", "needs_review", ""} {
		label := status
		if label == "" {
			label = "(unset, defaults to needs_review)"
		}
		func() {
			defer func() {
				// Expected: nil pool. Nothing to assert about the panic itself.
				_ = recover()
			}()
			rec := postLink(t, `{"bank_ids":["b1"],"gl_ids":["g1"],"status":"`+status+`"}`)
			if rec.Code == http.StatusBadRequest {
				t.Errorf("status %s was rejected by the whitelist: %s",
					label, problemDetail(t, rec))
			}
		}()
	}
}
