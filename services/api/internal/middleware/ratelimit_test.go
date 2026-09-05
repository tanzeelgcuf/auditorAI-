package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"
)

func TestRateLimitAllowsWithinBurst(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 3)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// burst = 3 allowed
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "1.2.3.4:5678"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
	// 4th exceeds burst -> 429
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("4th request: got %d, want 429", rec.Code)
	}
}

func TestRateLimitIsPerIP(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 1)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// IP A exhausts its burst
	for _, ip := range []string{"1.1.1.1:1", "1.1.1.1:2", "2.2.2.2:1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = ip
		h.ServeHTTP(rec, req)
	}
	// 1.1.1.1 done (burst 1), 2.2.2.2 was its own bucket -> allowed
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "2.2.2.2:1"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("1.1.1.1 with burst 1: got %d, want 429", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req2.RemoteAddr = "3.3.3.3:1" // fresh IP, fresh bucket
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("fresh IP 3.3.3.3: got %d, want 200", rec2.Code)
	}
}

// TestRateLimitIgnoresSpoofedXForwardedFor is the regression test for the bypass.
//
// It replaces TestRateLimitHonorsXForwardedFor, which asserted the OPPOSITE: that
// a shared X-Forwarded-For collapses two different peers into one bucket. That
// test passed, and what it pinned was the vulnerability — with no trusted proxy
// configured, honoring the header means a single peer can mint an unlimited
// number of buckets by varying it.
func TestRateLimitIgnoresSpoofedXForwardedFor(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 1)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// One peer, burst 1, a different forged XFF on each request. Without the fix
	// every one of these is a fresh bucket and all four return 200.
	codes := make([]int, 0, 4)
	for _, forged := range []string{"203.0.113.1", "203.0.113.2", "198.51.100.7", "8.8.8.8"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", forged)
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", codes[0])
	}
	for i, c := range codes[1:] {
		if c != http.StatusTooManyRequests {
			t.Errorf("request %d with forged XFF: got %d, want 429 — the header is "+
				"being honored from an untrusted peer, so per-IP limits are bypassable",
				i+2, c)
		}
	}
}

// A spoofed XFF must not let a caller borrow a DIFFERENT peer's bucket either:
// that direction is a denial-of-service against an innocent address.
func TestRateLimitSpoofCannotExhaustAnotherPeersBucket(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 1)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Attacker at 10.0.0.9 burns a bucket while claiming to be the victim.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.9:1"
		req.Header.Set("X-Forwarded-For", "203.0.113.50")
		h.ServeHTTP(rec, req)
	}
	// The victim, connecting for real, still has its own untouched bucket.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.50:5555"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("victim's first real request: got %d, want 200", rec.Code)
	}
}

// IPv4-mapped IPv6 and dotted IPv4 are the same host and must not get two
// buckets. Without canonicalIP this is a free doubling of any limit.
func TestRateLimitCanonicalisesIPv4MappedIPv6(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 1)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	first := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req1.RemoteAddr = "203.0.113.9:1"
	h.ServeHTTP(first, req1)
	if first.Code != http.StatusOK {
		t.Fatalf("dotted form: got %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req2.RemoteAddr = "[::ffff:203.0.113.9]:2"
	h.ServeHTTP(second, req2)
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("IPv4-mapped form of the same host: got %d, want 429", second.Code)
	}
}
