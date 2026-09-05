package middleware

// Per-IP token-bucket rate limiting (golang.org/x/time/rate).
//
// Replaces the no-op RateLimiter placeholder. Real, in-process
// limiter keyed by client IP; a gateway (Traefik/Kong) can do this more
// robustly in prod, but the API must not ship with NO limiting on auth/upload/
// admin endpoints.
//
// On the bucket map and memory: this comment used to claim the periodic sweep
// meant "an attacker can't exhaust memory by rotating IPs". That was false while
// clientIP trusted X-Forwarded-For — the key was an arbitrary caller-supplied
// string, so no IPs needed rotating at all, and entries are only evicted after
// ttl of IDLE time. The key is now the connection peer (see clientip.go), which
// costs an attacker a real address per bucket, and unparseable peers collapse to
// one shared key. Stated honestly: the map is bounded by the number of distinct
// source addresses seen in a 10-minute window, which a botnet or a routed IPv6
// /64 can still make large. A gateway limiter in front is the real answer; this
// one is the floor, not the ceiling.

import (
	"log/slog"
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

// clientIP is the rate-limit bucket key: the resolved client address, taken
// from r.RemoteAddr ONLY.
//
// It used to read X-Forwarded-For and prefer it over RemoteAddr, which made
// every limit in this service bypassable by sending a different value of a
// client-controlled header on each request. Header handling now lives in
// clientip.go behind a trusted-proxy check, and RealIP(tp) has already written
// the resolved address into RemoteAddr by the time this runs. Reading only
// RemoteAddr here is what keeps that a single decision point: if RealIP is ever
// dropped from the chain, this degrades to the true peer — safe — instead of
// silently re-trusting the caller.
//
// Returns "" only for input that is not an IP at all; RateLimit turns that into
// one shared bucket rather than a per-string bucket, so a malformed peer address
// cannot be used to allocate map entries.
func clientIP(r *http.Request) string {
	return peerIP(r)
}

// RateLimit returns middleware enforcing `limiter` per client IP. On exceeding
// the burst it returns 429 with a Retry-After header.
func RateLimit(limiter *IPRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if ip == "" {
				// Not a parseable address. Share one bucket rather than keying on
				// the raw string: an unbounded set of distinct keys is how the
				// bucket map becomes a memory-growth surface.
				ip = "unresolved"
			}
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

// Log a clear line when the limiter trips (ops visibility).
func logRateLimited(w http.ResponseWriter, r *http.Request, ip string) {
	slog.Warn("rate limited", "ip", ip, "path", r.URL.Path)
}
