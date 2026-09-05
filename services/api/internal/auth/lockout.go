package auth

// Per-account brute-force ceiling.
//
// WHY THIS EXISTS. Until 2026-09-04 the only brute-force control on this service
// was the per-IP token bucket in internal/middleware/ratelimit.go. That limit is
// real (see clientip.go — it was bypassable via X-Forwarded-For until the same
// day), but it is keyed on the SOURCE ADDRESS, so an attempt spread across many
// addresses is bounded only by how many addresses the attacker has. Against one
// known email address that is not a ceiling at all, and it left the six-digit
// second factor — 10^6, which is a small number — reachable by anyone who had
// already obtained the password.
//
// SHAPE OF THE POLICY, and the two rejected alternatives:
//   - Keyed on the ACCOUNT, not on (account, source address). A per-pair counter
//     has zero denial-of-service exposure but does not close the gap above: N
//     addresses buy N x threshold attempts. The pair variant was considered and
//     rejected for exactly that reason.
//   - The lock AUTO-EXPIRES and escalates in tiers rather than latching until an
//     operator clears it. A latching per-account lock is a denial-of-service
//     primitive against any address an attacker can guess; a tiered expiring lock
//     costs a griefer a sustained, rate-limited request stream to hold, and
//     releases the real user without a support ticket.
//
// SAME LOGIC, ONE PLACE. The tier arithmetic is here and nowhere else. It is not
// duplicated in SQL (infra/init.sql says so at the column definitions) and not
// duplicated in the handler. This repo has three separate incidents of a
// security-or-money rule existing in two languages and the copies drifting; see
// CLAUDE.md rules 8-10.
//
// Everything in this file is pure: no database, no HTTP, no time.Now(). `now` is
// injected so the boundaries are testable at the exact second, which is the only
// way the 30m/60m interaction below can be checked rather than assumed.

import (
	"errors"
	"time"
)

// ErrAccountLocked means the account is inside a lockout window. Handlers MUST
// NOT distinguish this from a bad password in what they send back — see the
// contract on IsLocked.
var ErrAccountLocked = errors.New("account temporarily locked")

const (
	// LockoutThreshold is the number of consecutive failures that first locks the
	// account. OWASP's testing guide puts the conventional range at 3-5; 5 is the
	// permissive end of it, chosen because the first tier is only 60 seconds and a
	// real user fat-fingering a password three times should not notice this exists.
	LockoutThreshold = 5

	// LockoutResetWindow is how long the account must go WITHOUT a failure before
	// the counter returns to zero.
	//
	// It is deliberately longer than lockoutMaxDuration. If they were equal, the
	// lock expiring and the counter resetting would coincide: an attacker parked on
	// the 30m tier would get the lock released AND the counter zeroed at the same
	// instant, buying 5 fresh attempts every 30 minutes instead of 1. The gap
	// between 60m and 30m is what makes the steady-state ceiling hold.
	LockoutResetWindow = 60 * time.Minute

	// lockoutMaxDuration caps the tier ladder. Beyond this a longer lock buys
	// almost nothing against an attacker and costs a real user real time.
	lockoutMaxDuration = 30 * time.Minute
)

// lockoutTiers maps a failure count to how long the account is locked. Read as
// "at or above `atLeast` failures, lock for `duration`", longest first.
//
// The ladder is evaluated on EVERY failure at or above the threshold, not only on
// the exact boundaries. That is the difference between a ceiling and a sieve: if
// only counts 5, 10, 15 and 20 locked, then 6, 7, 8 and 9 would be free guesses
// handed out the moment the 60-second lock expired.
var lockoutTiers = []struct {
	atLeast  int
	duration time.Duration
}{
	{atLeast: 20, duration: lockoutMaxDuration},
	{atLeast: 15, duration: 15 * time.Minute},
	{atLeast: 10, duration: 5 * time.Minute},
	{atLeast: LockoutThreshold, duration: 1 * time.Minute},
}

// LockoutState is the persisted failed-attempt state for one user, as read from
// the `users` row. A zero value means "clean: no failures on record".
type LockoutState struct {
	// FailedAttempts is users.failed_login_attempts.
	FailedAttempts int
	// LockedUntil is users.locked_until. Zero = not locked.
	LockedUntil time.Time
	// LastFailedAt is users.last_failed_login_at. Zero = never failed.
	LastFailedAt time.Time
}

// IsLocked reports whether the account is currently inside a lockout window.
//
// CALLER CONTRACT — this is the part that is easy to get wrong, and getting it
// wrong removes the control entirely:
//
//  1. When this returns true the caller MUST NOT verify the password, and MUST
//     return the same response it returns for a bad password. Verifying the
//     password while locked hands an attacker a password-correctness oracle that
//     does not consume the counter — they hammer through the lock window, watch
//     for the response that differs, and the lockout has bought nothing.
//  2. A failure that arrives while locked MUST NOT increment the counter (see
//     LockoutAfterFailure), so the lock stays bounded at its current tier instead
//     of being extendable by whoever is hammering it.
//
// Fails closed under clock skew: locked_until is written with the database's
// now() and compared here against the API process's clock, so a value that reads
// in the future keeps the account locked rather than releasing it early.
func IsLocked(st LockoutState, now time.Time) bool {
	if st.LockedUntil.IsZero() {
		return false
	}
	return now.Before(st.LockedUntil)
}

// LockoutAfterFailure computes the state to persist after ONE failed attempt.
//
// It is the only place the counter moves. Both a wrong password and a wrong or
// replayed second-factor code call it: counting only the password would let an
// attacker who already has the password switch to guessing the six-digit factor
// with no account-level ceiling.
//
// Returns the state to write. The caller writes it verbatim; there is no
// second decision to make afterwards.
func LockoutAfterFailure(st LockoutState, now time.Time) LockoutState {
	// Already locked: record nothing. Rule 2 of the IsLocked contract. The lock
	// is bounded at the tier it reached, and a caller who keeps failing during the
	// window cannot push it higher.
	if IsLocked(st, now) {
		return st
	}

	attempts := st.FailedAttempts
	// Idle reset. A user who mistyped four times last week starts clean; an
	// attacker has to sustain the attempt to keep the counter alive.
	if !st.LastFailedAt.IsZero() && !now.Before(st.LastFailedAt.Add(LockoutResetWindow)) {
		attempts = 0
	}
	// A negative or absurd stored value must not disable the ceiling. Treat
	// anything below zero as zero rather than trusting the column.
	if attempts < 0 {
		attempts = 0
	}
	attempts++

	next := LockoutState{FailedAttempts: attempts, LastFailedAt: now}
	if d := lockoutDuration(attempts); d > 0 {
		next.LockedUntil = now.Add(d)
	}
	return next
}

// lockoutDuration is the tier ladder. Returns 0 below the threshold.
func lockoutDuration(attempts int) time.Duration {
	for _, t := range lockoutTiers {
		if attempts >= t.atLeast {
			return t.duration
		}
	}
	return 0
}

// ClearedLockout is the state to persist on a login that ACTUALLY ISSUES TOKENS.
//
// "Actually issues tokens" is load-bearing and is the second trap in this file.
// Resetting on a correct password alone would defeat the second-factor ceiling
// completely: every TOTP guess in that attack carries the correct password, so
// the reset would fire immediately before each failed code and the counter would
// never leave 1. The reset belongs after the last gate, not after the first.
//
// An attempt that presents the right credentials but is refused for a reason that
// is not a guess — email not yet verified — neither increments nor clears. No
// guess failed, and a 403 path that silently zeroes the counter is a free reset
// for anyone who has the password.
func ClearedLockout() LockoutState {
	return LockoutState{}
}
