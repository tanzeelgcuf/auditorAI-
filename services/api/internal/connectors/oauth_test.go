package connectors

// oauth_test.go — the signed OAuth state, pure (no DB, no network).
//
// The state IS the public callback's authentication, so its properties are
// security-critical: a tampered user/book/expiry refuses, an expired state
// refuses, and a state signed for one provider/book/user triple does not
// verify against another.

import (
	"strings"
	"testing"
	"time"
)

func TestOAuthState_RoundTripVerifies(t *testing.T) {
	s := NewService()
	s.SetStateSecret([]byte("test-secret"))
	expiry := time.Now().Add(time.Minute)
	state := s.signOAuthState("user-1", "book-1", "quickbooks", expiry)
	user, book, ok := s.verifyOAuthState(state, "quickbooks")
	if !ok || user != "user-1" || book != "book-1" {
		t.Fatalf("round trip: ok=%v user=%q book=%q — a legitimately signed state did not verify", ok, user, book)
	}
}

func TestOAuthState_TamperedBookRefuses(t *testing.T) {
	s := NewService()
	s.SetStateSecret([]byte("test-secret"))
	state := s.signOAuthState("user-1", "book-1", "quickbooks", time.Now().Add(time.Minute))
	tampered := strings.Replace(state, "book-1", "book-2", 1)
	if _, _, ok := s.verifyOAuthState(tampered, "quickbooks"); ok {
		t.Fatal("a state with a tampered book id verified — the MAC is not enforced")
	}
}

func TestOAuthState_ExpiredRefuses(t *testing.T) {
	s := NewService()
	s.SetStateSecret([]byte("test-secret"))
	state := s.signOAuthState("user-1", "book-1", "quickbooks", time.Now().Add(-time.Minute))
	if _, _, ok := s.verifyOAuthState(state, "quickbooks"); ok {
		t.Fatal("an expired state verified — the expiry check is not enforced")
	}
}

func TestOAuthState_WrongProviderRefuses(t *testing.T) {
	s := NewService()
	s.SetStateSecret([]byte("test-secret"))
	state := s.signOAuthState("user-1", "book-1", "quickbooks", time.Now().Add(time.Minute))
	if _, _, ok := s.verifyOAuthState(state, "xero"); ok {
		t.Fatal("a state signed for quickbooks verified against xero — the provider is not bound into the MAC")
	}
}

func TestOAuthState_MalformedRefuses(t *testing.T) {
	s := NewService()
	s.SetStateSecret([]byte("test-secret"))
	for _, state := range []string{"", "no-pipes", "a|b|not-a-number|x", "a|b|123|"} {
		if _, _, ok := s.verifyOAuthState(state, "quickbooks"); ok {
			t.Errorf("malformed state %q verified", state)
		}
	}
}
