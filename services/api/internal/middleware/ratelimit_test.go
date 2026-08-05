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

func TestRateLimitHonorsXForwardedFor(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Limit(10), 1)
	h := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Same X-Forwarded-For across different RemoteAddr -> same bucket (burst 1)
	first := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req1.RemoteAddr = "10.0.0.1:1"
	req1.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(first, req1)
	if first.Code != http.StatusOK {
		t.Fatalf("first: got %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req2.RemoteAddr = "10.0.0.2:2" // different remote, same forwarded IP
	req2.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(second, req2)
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("same forwarded IP, burst 1: got %d, want 429", second.Code)
	}
}
