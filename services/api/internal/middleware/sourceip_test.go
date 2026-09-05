package middleware

// Tests for the source address recorded on every audited action.
//
// The property under test is NOT "a context value round-trips". It is that the
// address written to access_log is the same one ClientIP resolved and the rate
// limiter bucketed, and that it is the CLIENT's rather than the proxy's. Those
// two can only disagree through mount order, so the order is tested directly —
// including a negative control that builds the chain wrongly and asserts the
// wrong answer, because an ordering claim nobody has ever seen fail is not
// evidence.
//
// NOT COMPILED as of 2026-09-05: no Go toolchain in the environment that wrote
// these. Treat them as unverified until the first CI run.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// chain builds RealIP -> SourceIP (the production order) and returns whatever
// GetSourceIP saw at the handler.
func chain(r *http.Request, tp TrustedProxies) string {
	var got string
	h := RealIP(tp)(SourceIP(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		got = GetSourceIP(rr.Context())
	})))
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// reversed builds SourceIP -> RealIP: the mistake the CI guard exists to catch.
func reversed(r *http.Request, tp TrustedProxies) string {
	var got string
	h := SourceIP(RealIP(tp)(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		got = GetSourceIP(rr.Context())
	})))
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

func TestSourceIPRecordsPeerWhenNothingIsTrusted(t *testing.T) {
	tp, _ := ParseTrustedProxies("")
	// The header is present and must be ignored: with no trusted proxy the peer
	// IS the client, and believing the header here is the bypass that was fixed
	// in clientip.go.
	if got := chain(req("198.51.100.5:5555", "1.2.3.4"), tp); got != "198.51.100.5" {
		t.Errorf("source ip: got %q, want %q", got, "198.51.100.5")
	}
}

func TestSourceIPRecordsClientBehindTrustedProxy(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	if got := chain(req("10.0.0.7:443", "203.0.113.9"), tp); got != "203.0.113.9" {
		t.Errorf("source ip: got %q, want the client %q", got, "203.0.113.9")
	}
}

// The negative control. Mounted above RealIP, SourceIP reads the untouched
// RemoteAddr and records the PROXY — no error, no log line, a permanently wrong
// audit trail. scripts/check_audit_ip_arity.py pins the order in main.go because
// this failure is invisible at runtime; this test is what makes the claim
// "invisible" concrete rather than asserted.
func TestSourceIPMountedAboveRealIPRecordsTheProxy(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	// A FRESH request for each order. RealIP rewrites r.RemoteAddr on the request
	// it is handed, so reusing one here would feed the second chain an already
	// rewritten peer (203.0.113.9, no longer inside 10.0.0.0/8) and the second
	// assertion would pass for the wrong reason.
	if got := reversed(req("10.0.0.7:443", "203.0.113.9"), tp); got != "10.0.0.7" {
		t.Fatalf("expected the wrong order to yield the proxy %q, got %q — if this "+
			"now returns the client, the ordering constraint has changed and the CI "+
			"guard and both doc comments need revisiting", "10.0.0.7", got)
	}
	if got := chain(req("10.0.0.7:443", "203.0.113.9"), tp); got != "203.0.113.9" {
		t.Errorf("correct order: got %q, want %q", got, "203.0.113.9")
	}
}

func TestSourceIPIgnoresForgedHeaderFromUntrustedPeer(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	// The peer is not a trusted proxy, so its header is not evidence. An attacker
	// who could move the recorded address here could write someone else's IP into
	// the audit trail of their own action.
	if got := chain(req("198.51.100.5:5555", "10.0.0.7, 203.0.113.9"), tp); got != "198.51.100.5" {
		t.Errorf("source ip: got %q, want the peer %q", got, "198.51.100.5")
	}
}

func TestSourceIPMatchesClientIPExactly(t *testing.T) {
	// The audit trail and the rate limiter must never disagree about who called.
	// Both read one resolution; this asserts that rather than trusting it.
	tp, _ := ParseTrustedProxies("10.0.0.0/8, 172.18.0.5")
	cases := [][]string{
		{"10.0.0.7:443", "203.0.113.9"},
		{"10.0.0.7:443", "203.0.113.9", "10.0.0.8"},
		{"172.18.0.5:80", "  198.51.100.22  "},
		{"198.51.100.5:5555", "1.2.3.4"},
		{"10.0.0.7:443"},
		{"10.0.0.7:443", "10.0.0.9"},
		// IPv4-mapped IPv6 peer: one host must not have two spellings, or it gets
		// two rate-limit buckets and two identities in the audit trail.
		{"[::ffff:203.0.113.9]:443"},
		{"[2001:db8::1]:443"},
		// RemoteAddr that is not host:port at all. canonicalIP returns "", which
		// NULLIF($n,'')::inet stores as SQL NULL — an audit row that admits it does
		// not know, rather than one asserting a bogus address.
		{"garbage"},
	}
	for _, c := range cases {
		// Two identical requests: ClientIP is read from an untouched one because
		// RealIP mutates RemoteAddr on the request the chain runs against.
		want := ClientIP(req(c[0], c[1:]...), tp)
		if got := chain(req(c[0], c[1:]...), tp); got != want {
			t.Errorf("remote=%s xff=%v: audit recorded %q but ClientIP resolves %q",
				c[0], c[1:], got, want)
		}
	}
}

func TestGetSourceIPIsEmptyWithoutTheMiddleware(t *testing.T) {
	// Background workers, CLI tools and unit tests build a bare context. "" is the
	// documented answer and callers must turn it into SQL NULL.
	if got := GetSourceIP(context.Background()); got != "" {
		t.Errorf("bare context: got %q, want \"\"", got)
	}
	// A value stored under a DIFFERENT key type must not be returned as if it were
	// an address. This is the same mismatch that made both TOTP handlers 401 for
	// every caller: a key's dynamic type is part of its identity, so a same-named
	// key of another type never matches.
	type otherKey string
	ctx := context.WithValue(context.Background(), otherKey("source_ip"), "1.2.3.4")
	if got := GetSourceIP(ctx); got != "" {
		t.Errorf("a same-named key of another type must not be read as the source ip, got %q", got)
	}
}

func TestSourceIPUnparseableRemoteAddrIsEmpty(t *testing.T) {
	tp, _ := ParseTrustedProxies("")
	if got := chain(req("not-an-address"), tp); got != "" {
		t.Errorf("got %q, want \"\" so the column is NULL rather than garbage", got)
	}
}
