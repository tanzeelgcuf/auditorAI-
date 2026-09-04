package auth

// ORDERING INVARIANTS for HandleLogin's lockout wiring.
//
// READ THIS BEFORE TRUSTING THESE TESTS: they assert on the TEXT of auth.go, not
// on its behaviour. HandleLogin needs a *pgxpool.Pool, a real transaction and a
// user row, so exercising it needs Postgres; the behavioural equivalent belongs in
// the DATABASE_URL_TEST integration suite (see internal/middleware/security_test.go
// for the pattern) and DOES NOT EXIST. That is a gap, stated here rather than
// papered over. `go test` runs with the package directory as its working
// directory, so the file is read by plain name.
//
// WHY TEXT IS WORTH ASSERTING ON AT ALL. The two mistakes that would silently
// remove the per-account ceiling are both ORDERING mistakes, and neither one
// changes any pure function that lockout_test.go can reach:
//
//	1. Verifying the password while the account is locked. That hands an attacker
//	   a password-correctness oracle which does not consume the counter — they
//	   hammer through the window, watch for the response that differs, and the
//	   lockout has bought nothing.
//	2. Clearing the counter before the second factor has passed. Every TOTP guess
//	   in that attack carries the CORRECT password, so an early reset holds the
//	   counter at 1 forever and the 10^6 ceiling does not exist.
//
// Both look harmless in review — the first reads as a kindness to the user, the
// second as tidying. Pinning the order is the only cheap defence.
//
// NOT COMPILED. go and gofmt are absent from the sandbox this was written in. The
// assertion logic below was mirrored in Python and run three ways: 12/12 pass
// against the working tree, 12/12 fail against `git archive HEAD` (which predates
// the lockout), and — because most of those HEAD failures are only "symbol not
// found", which proves nothing but novelty — against five hand-built mutants of
// the CURRENT file in which every symbol is present and only the order, the arity
// or one call is wrong: (A) verify the password before consulting the lock, (B)
// clear the counter before the second-factor check, leaving the explanatory
// comment behind unmoved and now false, (C) drop one column from the SELECT and
// leave the Scan targets alone, (D) swap the last two Scan targets, (E) put the
// failed-attempt increment back on the request context. Each was caught by its own
// named invariant while the others stayed green, so these checks discriminate
// rather than blanket-failing. That is evidence about the assertions, not about
// the runtime.

import (
	"os"
	"strings"
	"testing"
)

// authCode returns auth.go with `//` line comments removed, so an invariant is
// not satisfied — or violated — by prose. HandleLogin's own comments describe the
// anti-patterns these tests forbid, which is exactly how a naive grep gets a
// false positive.
func authCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("read auth.go: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// handleLoginBody is the source of HandleLogin only, so an occurrence in some
// other handler cannot satisfy an ordering assertion about this one.
func handleLoginBody(t *testing.T) string {
	t.Helper()
	code := authCode(t)
	const sig = "func (s *Service) HandleLogin("
	start := strings.Index(code, sig)
	if start < 0 {
		t.Fatal("HandleLogin not found in auth.go")
	}
	rest := code[start+len(sig):]
	// The next top-level `func ` declaration ends the body.
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// indexOrFail returns the offset of needle, failing with the reason if absent.
func indexOrFail(t *testing.T, hay, needle, why string) int {
	t.Helper()
	i := strings.Index(hay, needle)
	if i < 0 {
		t.Fatalf("%q not found in HandleLogin: %s", needle, why)
	}
	return i
}

// INVARIANT 1 — the lock is checked before the password is verified.
func TestLockCheckPrecedesPasswordVerification(t *testing.T) {
	body := handleLoginBody(t)
	lock := indexOrFail(t, body, "IsLocked(", "the per-account lockout is not consulted at all")
	verify := indexOrFail(t, body, "VerifyPassword(", "the password is never verified")
	if lock > verify {
		t.Fatalf("VerifyPassword runs at offset %d, before IsLocked at %d: a locked account "+
			"still gets its password checked, which is an unlimited password-correctness "+
			"oracle that never consumes the counter", verify, lock)
	}
}

// INVARIANT 1b — the locked branch RETURNS. Checking the lock first is worthless
// if the branch falls through to the password compare.
func TestLockedBranchReturns(t *testing.T) {
	body := handleLoginBody(t)
	lock := indexOrFail(t, body, "if IsLocked(", "the lockout check is not a guard clause")
	verify := indexOrFail(t, body, "VerifyPassword(", "the password is never verified")
	branch := body[lock:verify]
	if !strings.Contains(branch, "return") {
		t.Fatal("no return between the IsLocked guard and VerifyPassword: the locked path " +
			"falls through and the password is compared anyway")
	}
	if !strings.Contains(branch, "invalidCredentials(w)") {
		t.Fatal("the locked path does not answer with invalidCredentials: a lockout that " +
			"responds differently from a bad password is a user-enumeration oracle")
	}
}

// INVARIANT 2 — the counter is cleared only after the second factor has passed.
func TestCounterResetHappensAfterTheSecondFactor(t *testing.T) {
	body := handleLoginBody(t)
	sf := indexOrFail(t, body, "CheckSecondFactor(", "the second factor is not checked")
	reset := indexOrFail(t, body, "failed_login_attempts = 0",
		"nothing ever clears the counter, so five typos lock a user out permanently")
	if reset < sf {
		t.Fatalf("the counter is cleared at offset %d, before CheckSecondFactor at %d. Every "+
			"TOTP guess carries the correct password, so this resets the counter before each "+
			"failed code and the six-digit ceiling does not exist", reset, sf)
	}
}

// INVARIANT 2b — tokens are issued only after that reset has been committed.
func TestTokensAreIssuedAfterTheCommit(t *testing.T) {
	body := handleLoginBody(t)
	reset := indexOrFail(t, body, "failed_login_attempts = 0", "nothing clears the counter")
	commit := indexOrFail(t, body, "tx.Commit(", "the successful login is never committed")
	tokens := indexOrFail(t, body, "GenerateTokens(", "no tokens are issued")
	if !(reset < commit && commit < tokens) {
		t.Fatalf("order is reset=%d commit=%d tokens=%d; want reset < commit < tokens so a "+
			"login cannot hand out tokens while the counter reset and the TOTP burn are "+
			"still uncommitted", reset, commit, tokens)
	}
}

// INVARIANT 3 — the row is locked for the read-check-increment.
func TestLoginRowIsSelectedForUpdate(t *testing.T) {
	body := handleLoginBody(t)
	sel := indexOrFail(t, body, "FROM users WHERE email = $1",
		"the login query no longer selects the user by email")
	forUpdate := indexOrFail(t, body, "FOR UPDATE",
		"the login SELECT has no FOR UPDATE: two concurrent attempts read the same failure "+
			"count and both write count+1, so the ceiling can be raised by parallelism")
	// Same statement, not some other query further down the handler.
	if forUpdate < sel || forUpdate-sel > 200 {
		t.Fatalf("FOR UPDATE is at offset %d and the user SELECT at %d: they are not the same "+
			"statement, so the row read for the lockout decision is not the row locked",
			forUpdate, sel)
	}
}

// INVARIANT 9 — the login SELECT and its Scan agree on arity and on the order of
// the three lockout columns.
//
// pgx scans BY POSITION into a `...any`, so adding a column to the SELECT without
// adding its target — or adding both in a different order — compiles cleanly and
// fails at runtime, or worse, silently reads locked_until into last_failed_login_at
// and produces an account that can never be locked. `go build` and `go vet` cannot
// see this. Three columns were appended to this query on 2026-09-04, which is
// exactly the edit that introduces it.
func TestLoginSelectAndScanAgree(t *testing.T) {
	body := handleLoginBody(t)
	sel := indexOrFail(t, body, "SELECT id, firm_id", "the login SELECT has changed shape")
	from := indexOrFail(t, body, "FROM users WHERE email = $1", "the login query changed")
	cols := splitTopLevel(body[sel+len("SELECT"):from])

	scan := indexOrFail(t, body, ".Scan(", "the login row is never scanned")
	end := strings.Index(body[scan:], ")\n")
	if end < 0 {
		t.Fatal("could not find the end of the Scan argument list")
	}
	args := splitTopLevel(body[scan+len(".Scan("):scan+end])

	if len(cols) != len(args) {
		t.Fatalf("the login SELECT has %d columns (%v) and Scan has %d targets (%v): pgx scans "+
			"by position, so this mismatch is a runtime error at best and a column read into "+
			"the wrong variable at worst", len(cols), cols, len(args), args)
	}
	wantTail := []string{"failed_login_attempts", "locked_until", "last_failed_login_at"}
	gotTail := cols[len(cols)-3:]
	for i := range wantTail {
		if gotTail[i] != wantTail[i] {
			t.Fatalf("the last three selected columns are %v, want %v in that order: the Scan "+
				"targets below assume it", gotTail, wantTail)
		}
	}
	wantArgs := []string{"&failedAttempts", "&lockedUntil", "&lastFailedAt"}
	gotArgs := args[len(args)-3:]
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Fatalf("the last three Scan targets are %v, want %v: locked_until being read into "+
				"last_failed_login_at gives an account that can never lock", gotArgs, wantArgs)
		}
	}
}

// splitTopLevel splits on commas that are not inside brackets, and trims each
// field to its last whitespace-separated token so `SELECT a,\n  b` and a
// multi-line Scan call both reduce to bare identifiers.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	var fields []string
	for _, f := range out {
		parts := strings.Fields(f)
		if len(parts) == 0 {
			continue
		}
		fields = append(fields, parts[len(parts)-1])
	}
	return fields
}

// INVARIANT 4 — every rejected login answers through the one helper. A second
// literal is how the locked response and the bad-password response drift apart.

func TestRejectionsGoThroughOneHelper(t *testing.T) {
	body := handleLoginBody(t)
	if strings.Contains(body, `"invalid email or password"`) {
		t.Fatal("HandleLogin contains the rejection string inline; it must go through " +
			"invalidCredentials so the unknown-email, wrong-password and locked responses " +
			"cannot diverge")
	}
	if n := strings.Count(body, "invalidCredentials(w)"); n < 3 {
		t.Fatalf("invalidCredentials is called %d times, want at least 3 (unknown email, "+
			"wrong password, locked account)", n)
	}
}

// INVARIANT 5 — both guessable factors increment, and the non-guess does not.
func TestBothFactorsCountAndAMissingCodeDoesNot(t *testing.T) {
	body := handleLoginBody(t)
	if n := strings.Count(body, "persistLoginFailure("); n < 2 {
		t.Fatalf("persistLoginFailure is called %d time(s), want 2: a wrong password AND a "+
			"wrong second-factor code must both count, or an attacker who has the password "+
			"switches to guessing the six digits with no account-level ceiling", n)
	}
	pwd := indexOrFail(t, body, `"bad_password"`, "the wrong-password failure is not recorded")
	totp := indexOrFail(t, body, `"bad_totp"`, "the wrong-TOTP failure is not recorded")
	if pwd > totp {
		t.Fatalf("the bad_password increment is at offset %d, after the bad_totp one at %d: the "+
			"password gate is supposed to run first, so this is not the login order the rest of "+
			"these invariants assume", pwd, totp)
	}

	// The TOTP increment must be guarded so a MISSING code is not counted: no
	// candidate secret was tested, and both shipped clients render the code as one
	// optional field beside the password, so an enrolled user submitting the form
	// empty is ordinary user error.
	sf := indexOrFail(t, body, "CheckSecondFactor(", "the second factor is not checked")
	between := body[sf:totp]
	if !strings.Contains(between, "ErrTOTPRequired") {
		t.Fatal("the bad_totp increment is not guarded by ErrTOTPRequired: a 2FA user who " +
			"submits the form with the code field empty is counted as a guess and gets " +
			"locked out for it")
	}
}

// INVARIANT 6 — the increment is committed. A failed-attempt counter that rolls
// back is a counter that does not exist, and HandleLogin's deferred Rollback makes
// that the DEFAULT outcome for anything written on a 401 path.
func TestFailureIncrementIsCommitted(t *testing.T) {
	code := authCode(t)
	const sig = "func (s *Service) persistLoginFailure("
	start := strings.Index(code, sig)
	if start < 0 {
		t.Fatal("persistLoginFailure not found in auth.go")
	}
	body := code[start:]
	if end := strings.Index(body[len(sig):], "\nfunc "); end >= 0 {
		body = body[:len(sig)+end]
	}
	if !strings.Contains(body, "tx.Commit(") {
		t.Fatal("persistLoginFailure never commits: HandleLogin's deferred Rollback discards " +
			"the increment and every attempt looks like the first")
	}
	if !strings.Contains(body, "failed_login_attempts") || !strings.Contains(body, "locked_until") {
		t.Fatal("persistLoginFailure does not write both counter columns")
	}
	if !strings.Contains(body, "if IsLocked(") {
		t.Fatal("persistLoginFailure does not refuse to write while locked: a failure inside " +
			"the window would extend it, so whoever is hammering the account decides how " +
			"long the real user stays out")
	}
}

// INVARIANT 7 — everything in HandleLogin runs on the transaction. A stray
// s.db.Exec would be outside the row lock the whole control depends on.
func TestLoginDoesNotBypassTheTransaction(t *testing.T) {
	body := handleLoginBody(t)
	for _, bad := range []string{"s.db.Exec(", "s.db.QueryRow(", "s.db.Query("} {
		if strings.Contains(body, bad) {
			t.Fatalf("HandleLogin calls %s directly: that statement is outside the FOR UPDATE "+
				"row lock, so it neither serialises against a concurrent attempt nor commits "+
				"with the rest of the decision", bad)
		}
	}
}

// INVARIANT 8 — a password reset clears the lockout. Without this a user who got
// locked out, assumed they had forgotten the password, and reset it stays refused
// with "invalid email or password" while holding a password they know is right.
func TestPasswordResetClearsTheLockout(t *testing.T) {
	code := authCode(t)
	const sig = "func (s *Service) HandleResetPassword("
	start := strings.Index(code, sig)
	if start < 0 {
		t.Fatal("HandleResetPassword not found in auth.go")
	}
	body := code[start:]
	if end := strings.Index(body[len(sig):], "\nfunc "); end >= 0 {
		body = body[:len(sig)+end]
	}
	for _, want := range []string{"failed_login_attempts", "locked_until", "last_failed_login_at"} {
		if !strings.Contains(body, want) {
			t.Fatalf("HandleResetPassword does not clear %s: a locked-out user who resets "+
				"their password is still locked out", want)
		}
	}
}

// INVARIANT 9 — the increment survives the client hanging up. `r.Context()` is
// cancelled the instant the socket closes, and persistLoginFailure's Exec AND its
// Commit both take a context. Left on the request context, an attacker who aborts
// each request the moment it is sent is never counted: every guess looks like the
// first and the ceiling stops existing. This is not an oracle —
// persistLoginFailure runs before invalidCredentials(w), so aborting forfeits the
// answer as well — but it is fail-OPEN, which is the shape that matters.
//
// The asymmetry is deliberate, so it is asserted in both directions: the SUCCESS
// path in HandleLogin must KEEP the request context, because losing that commit
// issues no tokens, burns no TOTP code and clears no counter, which is
// fail-CLOSED and correct.
func TestFailureIncrementSurvivesClientDisconnect(t *testing.T) {
	code := authCode(t)
	const sig = "func (s *Service) persistLoginFailure("
	start := strings.Index(code, sig)
	if start < 0 {
		t.Fatal("persistLoginFailure not found in auth.go")
	}
	body := code[start:]
	if end := strings.Index(body[len(sig):], "\nfunc "); end >= 0 {
		body = body[:len(sig)+end]
	}
	if !strings.Contains(body, "context.WithoutCancel(") {
		t.Fatal("persistLoginFailure does not strip cancellation: a caller who hangs up " +
			"cancels both the UPDATE and the Commit, the error is swallowed into a log " +
			"line, and the per-account ceiling silently stops existing")
	}
	if !strings.Contains(body, "context.WithTimeout(") {
		t.Fatal("persistLoginFailure strips cancellation without imposing a deadline: the " +
			"write then has no bound at all")
	}
	if strings.Contains(handleLoginBody(t), "context.WithoutCancel(") {
		t.Fatal("HandleLogin itself strips cancellation: the SUCCESS commit must stay " +
			"cancellable, or a client that disappears mid-login still burns its TOTP code " +
			"and clears its counter while receiving no token")
	}
}
