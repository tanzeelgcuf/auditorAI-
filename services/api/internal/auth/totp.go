package auth

import (
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"
)

// Second-factor decision logic, kept in a pure function so the security-critical
// part is testable without a database, an HTTP server, or a live clock. The
// handlers in auth.go do I/O; every accept/reject decision happens here.
//
// SCOPE — what this does and does not claim:
//   - It enforces the factor at login: a user whose `totp_secret` is set cannot
//     obtain tokens without a currently-valid code. Before 2026-09-04 the column
//     was written by /totp/verify and never read by anything, so enabling 2FA
//     changed nothing about how the account could be logged into.
//   - It rejects reuse of a code that has already been accepted, for as long as
//     that code could still pass validation. `totp.Validate` allows +/-1 30s step
//     (~90s of acceptance for one code), so without this a code observed once —
//     over a shoulder, in a screenshot, in a phished form — stays usable for the
//     rest of its window.
//   - It does NOT implement backup/recovery codes. A user who loses their
//     authenticator is locked out and needs an operator to clear `totp_secret`.
//     Deliberately deferred, and named here so it is not mistaken for done.
//   - It does NOT make 2FA mandatory for firm_admin. That is a policy gate on
//     enrollment, not on verification, and it is not implemented — see
//     SOC2_READINESS.md.
var (
	// ErrTOTPRequired means the account has a second factor and the request did
	// not carry a code. Callers must NOT issue tokens.
	ErrTOTPRequired = errors.New("two-factor code required")
	// ErrTOTPInvalid means a code was supplied and did not validate.
	ErrTOTPInvalid = errors.New("invalid two-factor code")
	// ErrTOTPReplay means the supplied code is one that was already accepted and
	// is still inside its acceptance window.
	ErrTOTPReplay = errors.New("two-factor code already used")
)

// totpReplayWindow is how long an accepted code is remembered as spent. It must
// be at least the span over which totp.Validate would still accept that code:
// one 30s step plus the default skew of one step on either side = 90s. 120s
// leaves margin for clock drift between the app and the database's now().
const totpReplayWindow = 120 * time.Second

// SecondFactorState is the persisted TOTP state for one user, as read from the
// `users` row. A zero value means "no second factor enrolled".
type SecondFactorState struct {
	// Secret is the live base32 TOTP secret. Empty = not enrolled.
	Secret string
	// LastCode is the most recent code this account successfully used.
	LastCode string
	// LastUsedAt is when LastCode was accepted. Zero if never.
	LastUsedAt time.Time
}

// NormalizeTOTPCode strips the separators authenticator apps and users add
// ("123 456", "123-456") and surrounding whitespace. It does not validate.
func NormalizeTOTPCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CheckSecondFactor decides whether a login attempt satisfies the account's
// second factor. It returns nil when the login may proceed.
//
// `now` is injected rather than read from time.Now() so the replay window is
// testable; handlers pass time.Now().UTC().
//
// FAIL-CLOSED CONTRACT: every path that is not an affirmative success returns a
// non-nil error. There is deliberately no branch that logs and continues — the
// bug this replaces was a column that was written and never read, and a
// "log and allow" path is the same failure with more noise.
func CheckSecondFactor(st SecondFactorState, submitted string, now time.Time) error {
	if st.Secret == "" {
		// Not enrolled. Nothing to verify; a code sent anyway is ignored rather
		// than treated as an error, because both shipped clients send the field
		// unconditionally.
		return nil
	}

	code := NormalizeTOTPCode(submitted)
	if code == "" {
		return ErrTOTPRequired
	}
	if len(code) != 6 {
		// Length-check before the HMAC so an oversized body cannot reach the
		// crypto path, and so "12345" reports invalid rather than being padded
		// or truncated by anything downstream.
		return ErrTOTPInvalid
	}

	// Replay check runs before validation: it is cheaper than an HMAC and gives a
	// distinct signal for the "same code twice" case. A code submitted after the
	// window has expired falls through to Validate, which will reject it because
	// the time step has moved on.
	if st.LastCode != "" && !st.LastUsedAt.IsZero() {
		// "Still spent" is `now < LastUsedAt + window`, which stays fail-closed
		// under clock skew: a LastUsedAt that reads in the FUTURE (the timestamp
		// is written with the database's now(), compared against the API
		// process's clock) keeps the code rejected rather than releasing it.
		fresh := now.Before(st.LastUsedAt.Add(totpReplayWindow))
		same := subtle.ConstantTimeCompare([]byte(code), []byte(st.LastCode)) == 1
		if fresh && same {
			return ErrTOTPReplay
		}
	}

	if !totp.Validate(code, st.Secret) {
		return ErrTOTPInvalid
	}
	return nil
}
