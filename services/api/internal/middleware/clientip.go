package middleware

// Client IP resolution, and why it is not one line.
//
// Every per-IP control in this service — the auth/upload/admin rate limiters —
// is only as strong as the answer to "which IP is this?". Before this file that
// answer came from TWO places, both of which trusted the caller:
//
//   1. `chimiddleware.RealIP` in the router chain rewrote r.RemoteAddr from
//      True-Client-IP / X-Real-IP / X-Forwarded-For with no notion of a trusted
//      proxy. chi's own docs say to use it only when those headers cannot be set
//      by the client.
//   2. ratelimit.go's clientIP read the LEFTMOST X-Forwarded-For element and
//      preferred it over RemoteAddr.
//
// The leftmost XFF element is the one the CLIENT writes; conforming proxies
// APPEND on the right. So `X-Forwarded-For: <anything>` produced a fresh token
// bucket per request and the 5 req/s ceiling on /v1/auth/login and
// /v1/totp/verify was not a ceiling at all. infra/docker-compose.yml publishes
// the api as 8080:8080 and no service carries traefik labels, so the header
// arrives from the internet untouched — this was not theoretical.
//
// The rule here: a forwarding header is evidence only if the machine that
// handed us the connection is one we chose to trust. With no trusted proxies
// configured we ignore the headers entirely, which fails CLOSED — clients
// behind an unconfigured proxy share a bucket (a capacity problem, loud and
// self-correcting) rather than each getting an unlimited private one (a
// security problem, silent).

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the set of peers whose forwarding headers we believe.
// A nil or empty set means "trust nothing", which is the zero value on purpose:
// a caller that forgets to configure this gets the safe behaviour.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies reads a comma-separated list of IPs and CIDRs, e.g.
//
//	"10.0.0.0/8, 172.18.0.5, ::1"
//
// A bare IP is treated as a /32 or /128. Unparseable entries are reported and
// skipped rather than silently dropped: a typo in this variable quietly widens
// or narrows an access-control boundary, so it must be visible at boot.
func ParseTrustedProxies(spec string) (TrustedProxies, []string) {
	var tp TrustedProxies
	var bad []string
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(entry); err == nil {
			tp.nets = append(tp.nets, n)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			bad = append(bad, entry)
			continue
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		tp.nets = append(tp.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return tp, bad
}

// Empty reports whether any proxy is trusted at all.
func (tp TrustedProxies) Empty() bool { return len(tp.nets) == 0 }

// Len is the number of configured entries (for boot logging).
func (tp TrustedProxies) Len() int { return len(tp.nets) }

// contains reports whether ip is a trusted proxy.
func (tp TrustedProxies) contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range tp.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// canonicalIP normalises an address so that two spellings of the same host
// cannot occupy two rate-limit buckets. In particular an IPv4-mapped IPv6
// address (`::ffff:203.0.113.9`) collapses to its dotted form, and invalid
// input returns "" so callers can reject rather than key a bucket on garbage.
func canonicalIP(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// peerIP is the address of the machine that actually opened the connection.
// This is the only input an attacker cannot forge, so everything else is
// measured against it.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr is not always host:port (httptest, some proxies, and chi's
		// RealIP used to strip the port). Fall back to the raw value.
		host = r.RemoteAddr
	}
	return canonicalIP(host)
}

// ClientIP resolves the caller's address.
//
// If the direct peer is not a trusted proxy, the peer IS the client and every
// forwarding header is ignored. If it is trusted, X-Forwarded-For is walked
// RIGHT TO LEFT — nearest hop first — skipping entries that are themselves
// trusted proxies, and the first remaining valid address is the client. Walking
// rightwards is what makes the result unspoofable: an attacker can prepend any
// number of fake hops on the left, but cannot remove the real one their own
// connection caused a trusted proxy to append.
//
// Falls back to the peer when the header is absent, unparseable, or made up
// entirely of trusted hops. Never returns "" for a well-formed request.
func ClientIP(r *http.Request, tp TrustedProxies) string {
	peer := peerIP(r)
	if tp.Empty() {
		return peer
	}
	if !tp.contains(net.ParseIP(peer)) {
		return peer
	}
	// X-Forwarded-For may appear more than once; Values gives them in order and
	// each may itself be a comma-separated list. Flatten, preserving order.
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if c := canonicalIP(part); c != "" {
				hops = append(hops, c)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !tp.contains(net.ParseIP(hops[i])) {
			return hops[i]
		}
	}
	// Every hop is a trusted proxy (or there were none). X-Real-IP is a
	// single-value header with no chain to reason about, so it is only usable
	// here, where we have already established the peer is trusted and XFF told
	// us nothing.
	if c := canonicalIP(r.Header.Get("X-Real-IP")); c != "" && !tp.contains(net.ParseIP(c)) {
		return c
	}
	return peer
}

// RealIP rewrites r.RemoteAddr to the resolved client address so that ordinary
// handlers, logs and any third-party middleware downstream see the right value
// without each having to know about proxies. It is the replacement for
// chimiddleware.RealIP and differs in exactly one way that matters: it does
// nothing at all unless the peer is a trusted proxy.
//
// The rewritten value keeps host:port shape, because that is the documented
// contract of RemoteAddr and net.SplitHostPort callers depend on it. The port is
// the peer's — the client's source port is not knowable from XFF.
func RealIP(tp TrustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !tp.Empty() {
				if resolved := ClientIP(r, tp); resolved != "" && resolved != peerIP(r) {
					_, port, err := net.SplitHostPort(r.RemoteAddr)
					if err != nil {
						port = "0"
					}
					r.RemoteAddr = net.JoinHostPort(resolved, port)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SourceIP stashes the resolved client address on the request context so that the
// audit writers can record "from where" without any of them resolving it again.
//
// MOUNT IT IMMEDIATELY AFTER RealIP AND NOWHERE ELSE. It reads peerIP(r), which
// by that point is RealIP's already-rewritten RemoteAddr, so the value stored is
// the same address ClientIP resolved and the same one the rate limiter bucketed.
// Mounted BEFORE RealIP it would silently record the proxy's address instead of
// the client's on every request behind a trusted proxy — no error, no log line,
// just a permanently wrong audit trail. router_order_test.go pins the ordering.
//
// It is a separate middleware rather than two extra lines inside RealIP because
// RealIP's documented contract is that it "does nothing at all unless the peer is
// a trusted proxy", and a context write on every request would falsify that.
//
// Note it does NOT skip the write when the value is empty: canonicalIP returns ""
// for an unparseable RemoteAddr, and storing "" is how SourceIPFrom's documented
// "" -> SQL NULL path gets exercised rather than a stale value being inherited.
func SourceIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), SourceIPKey, peerIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetSourceIP is the only way to read the value SourceIP wrote. It delegates to
// auth.SourceIPFrom, where the key is defined; both spellings exist so that
// middleware-package callers do not need to import auth for one accessor.
func GetSourceIP(ctx context.Context) string {
	if v, ok := ctx.Value(SourceIPKey).(string); ok {
		return v
	}
	return ""
}

// LogTrustedProxies states the resolved posture at boot. An empty set in
// production is not a security problem but it is a capacity one — every client
// behind the load balancer shares one bucket — so it is worth a warning rather
// than silence.
func LogTrustedProxies(tp TrustedProxies, bad []string, appEnv string) {
	for _, b := range bad {
		slog.Error("TRUSTED_PROXY_CIDRS: unparseable entry ignored", "entry", b)
	}
	if tp.Empty() {
		if appEnv == "production" {
			slog.Warn("TRUSTED_PROXY_CIDRS is empty in production: forwarding headers " +
				"are ignored, so every client behind a proxy shares one rate-limit bucket. " +
				"Set it to the proxy's address range.")
		} else {
			slog.Info("TRUSTED_PROXY_CIDRS empty: X-Forwarded-For ignored, peer address used")
		}
		return
	}
	slog.Info("trusted proxies configured", "entries", tp.Len())
}
