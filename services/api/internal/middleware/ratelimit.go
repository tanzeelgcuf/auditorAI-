package middleware

// Per-IP token-bucket rate limiting (golang.org/x/time/rate).
//
// Replaces the no-op RateLimiter placeholder (doc 00 §3.10). Real, in-process
// limiter keyed by client IP; a gateway (Traefik/Kong) can do this more
// robustly in prod, but the API must not ship with NO limiting on auth/upload/
// admin endpoints. Per-IP buckets live in a map with periodic sweep so an
// attacker can't exhaust memory by rotating IPs.

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiterEntry is one IP's token bucket + last-seen time (for GC).
type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// IPRateLimiter holds per-IP buckets with a background sweep.
type IPRateLimiter struct {
	mu       sync.Mutex
	entries  map[string]*limiterEntry
	rate     rate.Limit
	burst    int
	ttl      time.Duration // idle IPs evicted after this
}

// NewIPRateLimiter builds a limiter: `r` tokens/sec, `burst` max burst per IP.
func NewIPRateLimiter(r rate.Limit, burst int) *IPRateLimiter {
	rl := &IPRateLimiter{
		entries: map[string]*limiterEntry{},
		rate:    r,
		burst:   burst,
		ttl:     10 * time.Minute,
	}
	go rl.sweep()
	return rl
}

// get returns (or creates) the bucket for an IP.
func (rl *IPRateLimiter) get(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if e, ok := rl.entries[ip]; ok {
		e.lastSeen = time.Now()
		return e.limiter
	}
	l := rate.NewLimiter(rl.rate, rl.burst)
	rl.entries[ip] = &limiterEntry{limiter: l, lastSeen: time.Now()}
	return l
}

// sweep evicts idle IPs to bound memory.
func (rl *IPRateLimiter) sweep() {
	ticker := time.NewTicker(rl.ttl / 2)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		for ip, e := range rl.entries {
			if now.Sub(e.lastSeen) > rl.ttl {
				delete(rl.entries, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// clientIP extracts the caller IP, honoring X-Forwarded-For (set by a gateway)
// but falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := indexByte(xff, ','); i >= 0 {
			return trimSpace(xff[:i])
		}
		return trimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit returns middleware enforcing `limiter` per client IP. On exceeding
// the burst it returns 429 with a Retry-After header.
func RateLimit(limiter *IPRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !limiter.get(ip).Allow() {
				w.Header().Set("Retry-After", "1")
				logRateLimited(w, r, ip)
				writeProblem(w, r, "https://ai-auditor.dev/errors/rate-limited",
					"Too Many Requests", http.StatusTooManyRequests,
					"rate limit exceeded — try again shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Keep the old signature working if anything referenced RateLimiter directly;
// it was a no-op placeholder. Now it's a convenience: a conservative default
// limiter applied as middleware. Left exported to avoid breaking imports.
func RateLimiter(next http.Handler) http.Handler {
	return RateLimit(NewIPRateLimiter(10, 20))(next)
}

// Log a clear line when the limiter trips (ops visibility, doc 12 §3).
func logRateLimited(w http.ResponseWriter, r *http.Request, ip string) {
	slog.Warn("rate limited", "ip", ip, "path", r.URL.Path)
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
