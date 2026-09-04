package middleware

// Tests for client-IP resolution.
//
// The property under test is not "XFF is parsed" but "a caller cannot choose
// their own rate-limit identity". Every case is written from that angle: what
// does an attacker control, and does controlling it change the answer.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestParseTrustedProxies(t *testing.T) {
	tp, bad := ParseTrustedProxies("10.0.0.0/8, 172.18.0.5 , ::1, ,fd00::/8")
	if len(bad) != 0 {
		t.Fatalf("unexpected bad entries: %v", bad)
	}
	if tp.Len() != 4 {
		t.Errorf("entries: got %d, want 4 (empty element must be skipped, not counted)", tp.Len())
	}
	if tp.Empty() {
		t.Error("Empty() true with 4 entries")
	}

	// A typo must be REPORTED, not silently dropped: this variable is an
	// access-control boundary and a silent narrowing looks identical to working.
	tp2, bad2 := ParseTrustedProxies("10.0.0.0/8, not-an-ip, 999.1.1.1")
	if len(bad2) != 2 {
		t.Errorf("bad entries: got %v, want 2 reported", bad2)
	}
	if tp2.Len() != 1 {
		t.Errorf("valid entries kept: got %d, want 1", tp2.Len())
	}

	// The zero value must trust nothing.
	var zero TrustedProxies
	if !zero.Empty() {
		t.Error("zero-value TrustedProxies must be empty (trust nothing by default)")
	}
	if empty, _ := ParseTrustedProxies(""); !empty.Empty() {
		t.Error("empty spec must trust nothing")
	}
}

func TestClientIPIgnoresHeadersWithNoTrustedProxies(t *testing.T) {
	var none TrustedProxies
	got := ClientIP(req("198.51.100.4:9999", "1.2.3.4", "5.6.7.8"), none)
	if got != "198.51.100.4" {
		t.Errorf("got %q, want the peer 198.51.100.4 — headers must be ignored when "+
			"no proxy is trusted", got)
	}
}

func TestClientIPUsesForwardedWhenPeerIsTrusted(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	got := ClientIP(req("10.0.0.7:1", "203.0.113.9"), tp)
	if got != "203.0.113.9" {
		t.Errorf("got %q, want 203.0.113.9", got)
	}
}

// The core anti-spoof property. A client behind a trusted proxy can PREPEND
// hops; it cannot remove the one the proxy appended for its own connection. So
// the answer must come from the right.
func TestClientIPWalksRightToLeft(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	// Client at 203.0.113.9 sent "X-Forwarded-For: 1.1.1.1" hoping to be seen as
	// 1.1.1.1; the proxy appended the real address on the right.
	got := ClientIP(req("10.0.0.7:1", "1.1.1.1, 203.0.113.9"), tp)
	if got != "203.0.113.9" {
		t.Errorf("got %q, want 203.0.113.9 — a leftmost read is spoofable", got)
	}
}

func TestClientIPSkipsTrustedHops(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8, 172.16.0.0/12")
	// Two internal hops appended after the client: both are trusted, so the
	// client is the rightmost address that is NOT ours.
	got := ClientIP(req("10.0.0.7:1", "203.0.113.9, 172.16.4.4, 10.0.0.9"), tp)
	if got != "203.0.113.9" {
		t.Errorf("got %q, want 203.0.113.9", got)
	}
}

func TestClientIPHandlesRepeatedHeader(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	// Two separate X-Forwarded-For headers, order preserved; the real client is
	// the rightmost untrusted entry across BOTH.
	got := ClientIP(req("10.0.0.7:1", "1.1.1.1", "203.0.113.9, 10.0.0.9"), tp)
	if got != "203.0.113.9" {
		t.Errorf("got %q, want 203.0.113.9", got)
	}
}

func TestClientIPFallsBackToPeerOnUnusableHeader(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	cases := []struct {
		name string
		r    *http.Request
	}{
		{"no header", req("10.0.0.7:1")},
		{"garbage", req("10.0.0.7:1", "not-an-ip, also-not")},
		{"empty value", req("10.0.0.7:1", "")},
		{"only trusted hops", req("10.0.0.7:1", "10.0.0.5, 10.0.0.6")},
	}
	for _, c := range cases {
		if got := ClientIP(c.r, tp); got != "10.0.0.7" {
			t.Errorf("%s: got %q, want the peer 10.0.0.7", c.name, got)
		}
	}
}

func TestClientIPXRealIPOnlyFromTrustedPeer(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	// Untrusted peer: X-Real-IP must be ignored like every other header.
	r := req("198.51.100.4:1")
	r.Header.Set("X-Real-IP", "203.0.113.9")
	if got := ClientIP(r, tp); got != "198.51.100.4" {
		t.Errorf("untrusted peer: got %q, want 198.51.100.4", got)
	}
	// Trusted peer with no usable XFF: X-Real-IP is the only evidence available.
	r2 := req("10.0.0.7:1")
	r2.Header.Set("X-Real-IP", "203.0.113.9")
	if got := ClientIP(r2, tp); got != "203.0.113.9" {
		t.Errorf("trusted peer: got %q, want 203.0.113.9", got)
	}
	// XFF wins over X-Real-IP when both are present, because only XFF carries a
	// chain we can reason about.
	r3 := req("10.0.0.7:1", "198.51.100.77")
	r3.Header.Set("X-Real-IP", "203.0.113.9")
	if got := ClientIP(r3, tp); got != "198.51.100.77" {
		t.Errorf("both present: got %q, want 198.51.100.77", got)
	}
}

func TestCanonicalIP(t *testing.T) {
	// [in, want]. A host:port is deliberately NOT accepted: callers must split
	// first, so a bucket key can never be an address+port pair (which would give
	// one host as many buckets as it has source ports).
	for _, c := range [][2]string{
		{"203.0.113.9", "203.0.113.9"},
		{" 203.0.113.9 ", "203.0.113.9"},
		{"::ffff:203.0.113.9", "203.0.113.9"},
		{"2001:db8::1", "2001:db8::1"},
		{"2001:0db8:0000::0001", "2001:db8::1"},
		{"not-an-ip", ""},
		{"", ""},
		{"203.0.113.9:80", ""},
	} {
		if got := canonicalIP(c[0]); got != c[1] {
			t.Errorf("canonicalIP(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestPeerIPHandlesMissingPort(t *testing.T) {
	// httptest and some proxies hand over a bare address. It must still resolve
	// rather than becoming a distinct bucket key from the same host with a port.
	if got := peerIP(req("203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("bare address: got %q, want 203.0.113.9", got)
	}
	if got := peerIP(req("[2001:db8::1]:443")); got != "2001:db8::1" {
		t.Errorf("bracketed IPv6: got %q, want 2001:db8::1", got)
	}
	if got := peerIP(req("garbage")); got != "" {
		t.Errorf("unparseable peer: got %q, want \"\" so the caller can share one bucket", got)
	}
}

func TestRealIPRewritesOnlyForTrustedPeers(t *testing.T) {
	tp, _ := ParseTrustedProxies("10.0.0.0/8")
	var seen string
	h := RealIP(tp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	h.ServeHTTP(httptest.NewRecorder(), req("10.0.0.7:4321", "203.0.113.9"))
	if seen != "203.0.113.9:4321" {
		t.Errorf("trusted peer: RemoteAddr = %q, want 203.0.113.9:4321 (host:port shape "+
			"preserved so net.SplitHostPort callers keep working)", seen)
	}

	h.ServeHTTP(httptest.NewRecorder(), req("198.51.100.4:4321", "203.0.113.9"))
	if seen != "198.51.100.4:4321" {
		t.Errorf("untrusted peer: RemoteAddr = %q, want it untouched", seen)
	}

	var none TrustedProxies
	RealIP(none)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})).ServeHTTP(httptest.NewRecorder(), req("198.51.100.4:4321", "203.0.113.9"))
	if seen != "198.51.100.4:4321" {
		t.Errorf("no trusted proxies: RemoteAddr = %q, want it untouched", seen)
	}
}

// The end-to-end property, asserted through the two pieces as they are actually
// composed in main.go: RealIP then RateLimit. This is the test that would have
// caught the original bug, because it exercises the chain rather than either
// half in isolation.
func TestRealIPThenRateLimitIsNotSpoofable(t *testing.T) {
	var none TrustedProxies
	limiter := NewIPRateLimiter(1, 1)
	chain := RealIP(none)(RateLimit(limiter)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })))

	first := httptest.NewRecorder()
	chain.ServeHTTP(first, req("10.0.0.1:1", "1.1.1.1"))
	if first.Code != http.StatusOK {
		t.Fatalf("first: got %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	chain.ServeHTTP(second, req("10.0.0.1:1", "2.2.2.2"))
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("same peer, new forged XFF: got %d, want 429", second.Code)
	}
}
