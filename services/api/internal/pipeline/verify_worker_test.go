package pipeline

// Source-level invariants for VerifyWorker's disposition path.
//
// READ THIS BEFORE TRUSTING THESE TESTS: they assert on the TEXT of
// verify_worker.go, not on its behaviour. Exercising the real path needs
// Postgres, NATS and the Rust verification service, and a jetstream.Msg cannot
// be constructed outside a live connection. What these catch is a specific
// regression class this file has already suffered twice — a guard clause being
// dropped or an ack being moved — and they will not catch a bug that keeps the
// guards textually present. `go test` runs with the package directory as its
// working directory, so the file is read by plain name.
//
// The behavioural equivalent belongs in the DATABASE_URL_TEST integration suite
// (see internal/middleware/security_test.go for the pattern) and does not exist
// yet. That is a gap, stated here rather than papered over.

import (
	"os"
	"strings"
	"testing"
)

// codeOnly drops `//` line comments so an invariant is not satisfied — or
// violated — by prose. verify_worker.go's own comments quote the anti-patterns
// these tests forbid, which is exactly how a naive grep gets a false positive.
func codeOnly(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
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

func verifyWorkerCode(t *testing.T) string {
	t.Helper()
	return codeOnly(t, "verify_worker.go")
}

// TestDowngradeIsGuardedByAutoLinked — every status write in this worker must
// carry `AND status = 'auto_linked'`. Without it a redelivered event overwrites
// a human 'confirmed'/'rejected' decision, and a second delivery becomes a
// second state change instead of a 0-row no-op.
func TestDowngradeIsGuardedByAutoLinked(t *testing.T) {
	code := verifyWorkerCode(t)
	const stmt = "UPDATE reconciliation_groups SET status"
	n := strings.Count(code, stmt)
	if n == 0 {
		t.Fatal("no status write found; the deterministic tier can annotate but not dispose")
	}
	for i, part := range strings.Split(code, stmt)[1:] {
		// The guard must appear before the statement's closing backtick.
		end := strings.Index(part, "`")
		if end < 0 {
			t.Fatalf("status write %d: could not find the end of the SQL literal", i+1)
		}
		if !strings.Contains(part[:end], "AND status = 'auto_linked'") {
			t.Errorf("status write %d of %d is unguarded:\n%s",
				i+1, n, stmt+part[:end])
		}
	}
}

// TestWorkerNeverPromotes — exceeds_tolerance == false answers "the amounts
// reconcile", not "no human need look". A group sits in review for reasons this
// tier cannot see (date window, counterparty similarity, a pass-5 mismatch
// flag). Promotion here would silently retire human review.
func TestWorkerNeverPromotes(t *testing.T) {
	code := verifyWorkerCode(t)
	for _, forbidden := range []string{
		"SET status = 'auto_linked'",
		"SET status = 'confirmed'",
		"SET status = 'rejected'",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("verify_worker.go contains %q; this worker downgrades only", forbidden)
		}
	}
}

// TestFindingInsertIsIdempotent — redelivery must not write a second finding for
// the same group. The INSERT is a SELECT … WHERE NOT EXISTS rather than an
// ON CONFLICT, because there is no unique constraint on
// audit_findings.reconciliation_group_id.
func TestFindingInsertIsIdempotent(t *testing.T) {
	code := verifyWorkerCode(t)
	i := strings.Index(code, "INSERT INTO audit_findings")
	if i < 0 {
		t.Fatal("no audit_findings INSERT found; this worker is the only writer of findings")
	}
	end := strings.Index(code[i:], "`")
	if end < 0 {
		t.Fatal("could not find the end of the audit_findings SQL literal")
	}
	stmt := code[i : i+end]
	if !strings.Contains(stmt, "NOT EXISTS") {
		t.Errorf("the finding INSERT is not idempotent; a redelivered event writes a "+
			"duplicate finding:\n%s", stmt)
	}
	if !strings.Contains(stmt, "reconciliation_group_id = $2") {
		t.Errorf("the NOT EXISTS guard does not key on reconciliation_group_id:\n%s", stmt)
	}
}

// TestFindingAndDispositionShareOneTransaction — the failure mode of splitting
// them is precisely the state being fixed: an over-tolerance finding recorded
// while the group still reads as reconciled.
func TestFindingAndDispositionShareOneTransaction(t *testing.T) {
	code := verifyWorkerCode(t)
	insert := strings.Index(code, "INSERT INTO audit_findings")
	commit := strings.Index(code, "tx.Commit(ctx)")
	update := strings.LastIndex(code, "UPDATE reconciliation_groups SET status")
	begin := strings.Index(code, "w.db.Begin(ctx)")
	if insert < 0 || commit < 0 || update < 0 || begin < 0 {
		t.Fatalf("missing landmark: begin=%d insert=%d update=%d commit=%d",
			begin, insert, update, commit)
	}
	if !(begin < insert && insert < update && update < commit) {
		t.Errorf("the finding and the disposition are not both inside one transaction: "+
			"begin=%d insert=%d update=%d commit=%d", begin, insert, update, commit)
	}
	if !strings.Contains(code, "tx.Exec(ctx,") {
		t.Error("the writes do not run on the transaction handle")
	}
	if !strings.Contains(code, "tx.Rollback(ctx)") {
		t.Error("no rollback on the error paths")
	}
}

// TestNoAckBeforeWork — the ack-before-work class, found in all three consumers.
// `defer msg.Ack()` at the top of a handler acknowledges the event before the
// work succeeds, so a DB blip or a verification outage destroys it and NO
// FINDING IS EVER WRITTEN. The group then reads as linked with nothing flagged,
// which is indistinguishable from "reconciled cleanly".
func TestNoAckBeforeWork(t *testing.T) {
	code := verifyWorkerCode(t)
	if strings.Contains(code, "defer msg.Ack") {
		t.Error("`defer msg.Ack()` is back: the event is acknowledged before the " +
			"finding is written")
	}
	if strings.Contains(code, "defer func() { _ = msg.Ack") {
		t.Error("a deferred ack in a closure is the same defect wearing a hat")
	}
	commit := strings.Index(code, "tx.Commit(ctx)")
	ack := strings.LastIndex(code, "msg.Ack()")
	if commit < 0 || ack < 0 {
		t.Fatalf("missing landmark: commit=%d ack=%d", commit, ack)
	}
	if ack < commit {
		t.Errorf("the success-path ack (%d) precedes the commit (%d)", ack, commit)
	}
}

// TestPresenceComesFromMembership — a leg is present iff it has ≥1 member.
// Deriving presence from a non-zero total instead makes an invoice plus its full
// credit note (netting to 0) invisible, and the same divergence existed in
// link.py's is_exact and graph_def.py's _verify_node. The Rust side builds a leg
// only `if req.has_X` (grpc/mod.rs:130-141), so a false flag means the leg is
// not compared at all.
func TestPresenceComesFromMembership(t *testing.T) {
	code := verifyWorkerCode(t)
	for _, role := range []string{"invoice", "bank", "gl"} {
		want := "BOOL_OR(m.role='" + role + "')"
		if !strings.Contains(code, want) {
			t.Errorf("presence for the %s leg is not derived from membership (%s missing)",
				role, want)
		}
	}
	if strings.Contains(code, "!= 0") && strings.Contains(code, "hasInv") {
		t.Error("presence looks like it is being derived from a non-zero total")
	}
	if !strings.Contains(code, "HasInvoice:") || !strings.Contains(code, "HasBank:") ||
		!strings.Contains(code, "HasGl:") {
		t.Error("the presence flags are not sent on the gRPC request; proto3 defaults " +
			"an unset bool to false, which makes every leg absent and every group clean")
	}
}
