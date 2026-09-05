package auth

// Tests for the per-account brute-force ceiling in lockout.go.
//
// WHAT THESE PROVE, AND WHAT THEY DO NOT. Everything here is pure logic: the tier
// ladder, the idle reset, the clock-skew posture, and the arithmetic of the
// steady-state ceiling. None of it touches a database, so none of it proves that
// HandleLogin calls these functions in the right ORDER — the two mistakes that
// would silently remove the control (verifying the password while locked, and
// clearing the counter before the second factor) are ordering mistakes in the
// handler, and they are pinned separately in login_lockout_test.go.
//
// These tests have NOT been compiled. go and gofmt are absent from the sandbox
// this was written in. To stop that from meaning "unverified", every assertion
// below was transcribed into a Python mirror of lockout.go and RUN: 13/13 pass,
// and five mutations of the mirror (reset window equal to the cap, a ladder that
// fires only on exact boundaries, failures counted during the window, a trusted
// negative counter, and a lock released by API-clock skew) were each caught by
// the correspondingly named test, so the suite is not vacuous. That exercise
// found a real defect here: TestSteadyStateCeilingIsOneAttemptPerCap originally
// asserted a range of 40-60 attempts per 24h and the measured figure is 64.
// The numbers below are therefore measured; the Go runtime behaviour is not.

import (
	"testing"
	"time"
)

// t0 is a fixed instant so every duration in these tests is exact.
var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func TestCleanStateIsNotLocked(t *testing.T) {
	if IsLocked(LockoutState{}, t0) {
		t.Fatal("a zero LockoutState must not be locked; a fresh account would be unable to log in")
	}
}

// The threshold is the boundary that decides whether a real user notices this
// feature at all, so it is asserted attempt by attempt rather than in bulk.
func TestFailuresBelowThresholdDoNotLock(t *testing.T) {
	st := LockoutState{}
	for i := 1; i < LockoutThreshold; i++ {
		st = LockoutAfterFailure(st, t0)
		if st.FailedAttempts != i {
			t.Fatalf("after %d failures the counter reads %d", i, st.FailedAttempts)
		}
		if !st.LockedUntil.IsZero() {
			t.Fatalf("failure %d of %d locked the account; the threshold is not being respected",
				i, LockoutThreshold)
		}
		if IsLocked(st, t0) {
			t.Fatalf("IsLocked is true at %d failures, below the threshold of %d", i, LockoutThreshold)
		}
	}
}

func TestThresholdFailureLocksForOneMinute(t *testing.T) {
	st := LockoutState{}
	for i := 0; i < LockoutThreshold; i++ {
		st = LockoutAfterFailure(st, t0)
	}
	if !IsLocked(st, t0) {
		t.Fatalf("%d failures did not lock the account", LockoutThreshold)
	}
	if got := st.LockedUntil.Sub(t0); got != time.Minute {
		t.Fatalf("first tier locked for %v, want 1m", got)
	}
	// The boundary itself: the lock must be over AT locked_until, not after it.
	if IsLocked(st, st.LockedUntil) {
		t.Fatal("still locked at exactly locked_until; the window is longer than it claims")
	}
	if !IsLocked(st, st.LockedUntil.Add(-time.Nanosecond)) {
		t.Fatal("not locked one nanosecond before locked_until")
	}
}

// The tier ladder, asserted as a table. These are the durations the SOC2 row and
// the init.sql comment both quote, so a change here is a documentation change too.
func TestTierLadder(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 0}, {4, 0},
		{5, time.Minute}, {6, time.Minute}, {9, time.Minute},
		{10, 5 * time.Minute}, {14, 5 * time.Minute},
		{15, 15 * time.Minute}, {19, 15 * time.Minute},
		{20, 30 * time.Minute}, {200, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := lockoutDuration(c.attempts); got != c.want {
			t.Errorf("lockoutDuration(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// THE SIEVE TEST. If only the exact boundaries 5, 10, 15, 20 locked, then the
// moment a 60-second lock expired the attacker would get attempts 6, 7, 8 and 9
// for free. Every failure at or above the threshold must re-lock.
func TestEveryFailureAtOrAboveThresholdRelocks(t *testing.T) {
	now := t0
	st := LockoutState{}
	for i := 0; i < LockoutThreshold; i++ {
		st = LockoutAfterFailure(st, now)
	}
	// Walk the next 14 attempts, each one arriving the instant its lock expires.
	for n := LockoutThreshold + 1; n <= 19; n++ {
		now = st.LockedUntil
		if IsLocked(st, now) {
			t.Fatalf("attempt %d: still locked at locked_until", n)
		}
		st = LockoutAfterFailure(st, now)
		if st.FailedAttempts != n {
			t.Fatalf("attempt %d: counter reads %d", n, st.FailedAttempts)
		}
		if !IsLocked(st, now) {
			t.Fatalf("attempt %d did not re-lock: an attacker gets this guess for free "+
				"and every other one up to the next tier boundary", n)
		}
	}
}

// A failure inside the window must not extend it. Otherwise whoever is hammering
// the account decides how long the real user stays out, and the lock is unbounded
// in practice even though every individual tier is bounded.
func TestFailureWhileLockedChangesNothing(t *testing.T) {
	st := LockoutState{}
	for i := 0; i < LockoutThreshold; i++ {
		st = LockoutAfterFailure(st, t0)
	}
	locked := st

	for _, at := range []time.Time{
		t0.Add(time.Second),
		t0.Add(30 * time.Second),
		locked.LockedUntil.Add(-time.Nanosecond),
	} {
		got := LockoutAfterFailure(locked, at)
		if got.FailedAttempts != locked.FailedAttempts {
			t.Errorf("failure at %v moved the counter %d -> %d while locked",
				at.Sub(t0), locked.FailedAttempts, got.FailedAttempts)
		}
		if !got.LockedUntil.Equal(locked.LockedUntil) {
			t.Errorf("failure at %v extended the lock %v -> %v",
				at.Sub(t0), locked.LockedUntil, got.LockedUntil)
		}
	}
}

func TestIdleResetClearsTheCounter(t *testing.T) {
	st := LockoutState{FailedAttempts: 4, LastFailedAt: t0}

	// One nanosecond short of the window: still counted.
	almost := LockoutAfterFailure(st, t0.Add(LockoutResetWindow-time.Nanosecond))
	if almost.FailedAttempts != 5 {
		t.Fatalf("just inside the reset window the counter reads %d, want 5", almost.FailedAttempts)
	}

	// At the window: reset, so this failure is the first one again.
	at := LockoutAfterFailure(st, t0.Add(LockoutResetWindow))
	if at.FailedAttempts != 1 {
		t.Fatalf("at the reset window the counter reads %d, want 1", at.FailedAttempts)
	}
	if !at.LockedUntil.IsZero() {
		t.Fatal("a reset counter locked on its first failure")
	}
}

// THE REASON LockoutResetWindow > lockoutMaxDuration.
//
// Park an attacker on the top tier, let the lock expire, and fail again at exactly
// that instant. If the reset window equalled the 30m cap, the lock expiring and
// the counter zeroing would coincide: the attacker would get 5 fresh attempts
// every 30 minutes instead of 1, a 5x weaker steady-state ceiling. The assertion
// is that the counter keeps climbing.
func TestResetWindowOutlastsTheLongestLock(t *testing.T) {
	if LockoutResetWindow <= lockoutMaxDuration {
		t.Fatalf("LockoutResetWindow (%v) must be strictly greater than the longest lock (%v), "+
			"or the lock expiring and the counter resetting coincide", LockoutResetWindow, lockoutMaxDuration)
	}

	now := t0
	st := LockoutState{}
	for st.FailedAttempts < 20 {
		if IsLocked(st, now) {
			now = st.LockedUntil
		}
		st = LockoutAfterFailure(st, now)
	}
	if got := st.LockedUntil.Sub(now); got != lockoutMaxDuration {
		t.Fatalf("at %d attempts the lock is %v, want the %v cap", st.FailedAttempts, got, lockoutMaxDuration)
	}

	// The next attempt, at the instant the cap expires.
	before := st.FailedAttempts
	now = st.LockedUntil
	st = LockoutAfterFailure(st, now)
	if st.FailedAttempts != before+1 {
		t.Fatalf("counter went %d -> %d when the top-tier lock expired; the reset window is "+
			"firing at the same moment as the lock and handing back %d free attempts",
			before, st.FailedAttempts, LockoutThreshold)
	}
	if got := st.LockedUntil.Sub(now); got != lockoutMaxDuration {
		t.Fatalf("re-lock after the cap expired is %v, want %v", got, lockoutMaxDuration)
	}
}

// THE MEASURED CEILING. This number is not an estimate — the identical loop was
// run in the Python mirror and printed 64, and the first draft of this test
// asserted a range of 40-60 and was WRONG. Recorded exactly so a future change to
// the ladder has to argue with a number instead of a feeling.
//
// Where 64 comes from: attempts 1-5 all land at t=0 (nothing below the threshold
// locks), and after that the gap before attempt n is lockoutDuration(n-1) — 5
// gaps of 1m puts attempt 10 at t=5m, 5 of 5m puts attempt 15 at t=30m, 5 of 15m
// puts attempt 20 at t=105m, and the remaining (1440-105)/30 = 44 arrive at the
// 30m cap. 20+44 = 64.
//
// Against the six-digit second factor that is 10^6/64 ≈ 15,600 days to walk the
// whole space, which is the ceiling this control exists to create.
func TestSteadyStateCeilingIsOneAttemptPerCap(t *testing.T) {
	now := t0
	st := LockoutState{}
	deadline := t0.Add(24 * time.Hour)
	var at []time.Time
	for {
		if IsLocked(st, now) {
			now = st.LockedUntil
		}
		if now.After(deadline) {
			break
		}
		st = LockoutAfterFailure(st, now)
		at = append(at, now)
	}
	if len(at) != 64 {
		t.Fatalf("an attacker who never pauses gets %d attempts in 24h, want exactly 64; "+
			"the ladder changed and the documented ceiling did not", len(at))
	}
	// Steady state is the part that matters: past the ladder, exactly one attempt
	// per cap. A second distinct gap here means some tier is leaking extra guesses.
	for i := 19; i < len(at)-1; i++ {
		if got := at[i+1].Sub(at[i]); got != lockoutMaxDuration {
			t.Fatalf("gap between attempts %d and %d is %v, want the %v cap: steady state is "+
				"no longer one attempt per lock", i+1, i+2, got, lockoutMaxDuration)
		}
	}
	t.Logf("attempts admitted in 24h against one account: %d", len(at))
}

// Clock skew. locked_until is written with the DATABASE's now() and compared
// against the API process's clock, so the two can disagree. Disagreement must
// keep the account locked, never release it early.
func TestClockSkewFailsClosed(t *testing.T) {
	st := LockoutState{FailedAttempts: 5, LockedUntil: t0.Add(time.Hour), LastFailedAt: t0}
	if !IsLocked(st, t0.Add(-time.Hour)) {
		t.Fatal("an API clock running an hour behind the database released the lock")
	}
	if !IsLocked(st, t0) {
		t.Fatal("locked_until an hour in the future did not read as locked")
	}
}

// A stored counter that is absurd must not switch the ceiling off. Nothing in this
// service writes a negative value, which is exactly why an assumption about it
// would go unnoticed.
func TestNegativeStoredCounterIsTreatedAsZero(t *testing.T) {
	got := LockoutAfterFailure(LockoutState{FailedAttempts: -100, LastFailedAt: t0}, t0)
	if got.FailedAttempts != 1 {
		t.Fatalf("a stored counter of -100 produced %d after one failure, want 1", got.FailedAttempts)
	}
}

func TestClearedLockoutIsFullyClean(t *testing.T) {
	got := ClearedLockout()
	if got.FailedAttempts != 0 || !got.LockedUntil.IsZero() || !got.LastFailedAt.IsZero() {
		t.Fatalf("ClearedLockout is not clean: %+v", got)
	}
	if IsLocked(got, t0) {
		t.Fatal("ClearedLockout reads as locked")
	}
}

// A cleared account starts the ladder over, rather than resuming near a tier
// boundary. If LastFailedAt survived the clear, the next single failure would be
// evaluated against a stale timestamp.
func TestClearedAccountStartsTheLadderOver(t *testing.T) {
	st := LockoutAfterFailure(ClearedLockout(), t0)
	if st.FailedAttempts != 1 || !st.LockedUntil.IsZero() {
		t.Fatalf("first failure after a clear produced %+v", st)
	}
}
