package auth

// token_jti_test.go — the jti guard.
//
// SOURCE-INVARIANT: HandleLogin/HandleRefresh need a live Postgres, so the
// behavioural equivalent ("one logout denies every refresh") cannot run here;
// this asserts on the TEXT of auth.go, the same trade login_lockout_test.go
// makes.
//
// The bug it pins (read from the code 2026-09-28, the denylist build's first
// finding): no token was EVER issued with a jti claim — every token carried
// jti "" — so HandleLogout's denyToken(claims.ID) denied the EMPTY string and
// HandleRefresh's isTokenDenied("") returned true for EVERY refresh token in
// the deployment: one logout, anywhere, denied every refresh until the
// process restarted. The 09-05 status doc's framing ("a revoked token becomes
// valid again after a restart") had the direction backwards. The fix: every
// issuance site sets ID: uuid.NewString(), and isTokenDenied's empty-jti
// guard (return false) prevents the catastrophe even if a site is missed —
// the missed token is then merely undenyable, not a global lockout.
//
// Non-vacuity: run against the pre-fix tree (git archive HEAD, copy this file
// in) — the assertion fails there, because no RegisteredClaims construction
// in pre-fix auth.go carries ID.

import (
	"os"
	"strings"
	"testing"
)

func TestEveryTokenIssuanceCarriesAJti(t *testing.T) {
	raw, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("auth.go unreadable: %v", err)
	}
	src := string(raw)
	lines := strings.Split(src, "\n")
	checked := 0
	for i, line := range lines {
		if !strings.Contains(line, "jwt.RegisteredClaims{") {
			continue
		}
		// Look ahead through the struct literal's fields for the jti.
		window := strings.Join(lines[i:min(i+12, len(lines))], "\n")
		checked++
		if !strings.Contains(window, "ID:        uuid.NewString()") &&
			!strings.Contains(window, "ID: uuid.NewString()") {
			t.Errorf("auth.go:%d: a token issuance site sets no jti — every token would share the empty jti and one logout denies every refresh in the deployment", i+1)
		}
	}
	if checked < 5 {
		t.Fatalf("expected at least 5 token issuance sites in auth.go, found %d — the issuances were removed or renamed and this guard now covers nothing", checked)
	}
}
