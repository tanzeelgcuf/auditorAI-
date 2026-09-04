#!/usr/bin/env python3
"""Guard: an audit or security write must not be cancellable by the party it records.

THE BUG CLASS. `r.Context()` is cancelled the instant the client's socket closes.
A database write performed on it whose error is SWALLOWED INTO A LOG LINE can
therefore be deleted by the caller simply by hanging up — and nothing anywhere
reports a failure that mattered, because the action being recorded has already
committed. A write whose error IS returned to the caller is not in this class:
losing it fails the request, which is visible.

FOUR INSTANCES SO FAR, three of them pre-existing:
  1. middleware.ReleaseRLSConn — the `RESET app.current_firm` was a silent no-op
     on exactly the requests most likely to be aborted mid-flight, so the tenant
     GUCs survived into the next request that acquired that pooled connection.
     Fixed before this guard existed; it is the precedent the others follow.
  2. auth.persistLoginFailure (found 2026-09-04) — the failed-login increment IS
     the per-account lockout ceiling. Cancel it and the ceiling stops existing.
  3. middleware.RecordAccess (2026-09-04) — all 12 call sites pass `r.Context()`,
     so a caller could drop their own `access_log` row by hanging up.
  4. humanoverride.LogConfigChange (2026-09-04) — same shape, `config_change_log`.

THE FIX SHAPE, in all four:
    ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancel()
`WithoutCancel` keeps the ctx VALUES (RecordAccess needs them to find the
RLS-wired request connection) and drops only cancellation; the bounded deadline
replaces the one it dropped. The pooled DB connection has nothing to do with the
client's socket, so it stays usable.

WHAT THIS SCRIPT CANNOT DO. It reads source text. It cannot tell which writes are
security- or audit-relevant, and it cannot follow a context through a helper that
renames it. It flags candidates for a human to judge and then records that
judgement in ALLOWLIST. It is deliberately conservative in one direction only: it
looks 25 lines ahead for the write's own error guard, so a swallow further away
than that is missed.

Exit 0 = no unaccounted candidates and every known fix still in place.
Exit 1 = a new candidate, a reverted fix, or the guard has gone inert.
"""

import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Sites that MUST carry the strip, keyed "relpath::func". This half of the guard
# is a POSITIVE assertion and it is the half that survives regex drift: if the
# patterns below ever stop matching this codebase, the negative half quietly
# reports "nothing to see" while these four are the reason the guard exists.
# Reverting any one of them fails CI by name.
MUST_STRIP = {
    "services/api/internal/middleware/middleware.go::ReleaseRLSConn": (
        "Resets app.current_firm / app.assigned_books before the connection goes "
        "back to the pool. Cancelled = the GUCs of one tenant survive into the "
        "next request that acquires this connection. Cross-tenant, not just audit."
    ),
    "services/api/internal/auth/auth.go::persistLoginFailure": (
        "Writes the failed-login increment that IS the per-account lockout "
        "ceiling. Cancelled = no ceiling. Fail-open by definition."
    ),
    "services/api/internal/middleware/auditlog.go::RecordAccess": (
        "The only writer of access_log, and all 12 call sites pass r.Context(), "
        "so the recorded party held the cancel button."
    ),
    "services/api/internal/humanoverride/humanoverride.go::LogConfigChange": (
        "The only writer of config_change_log. Same shape: the person changing a "
        "book's settings could suppress the record of the change."
    ),
}

# Candidates judged OUT of the class, keyed the same way. Each entry is a claim
# that must stay true. Note the second reason in each: for these three the ctx
# cancellation is the SHUTDOWN signal, so stripping it would be actively wrong —
# it would keep a draining process writing after it was told to stop.
ALLOWLIST = {
    "services/api/internal/notify/notify.go::runOnce": (
        "Background reminder sweep. Started once as `go notify.Run(ctx, ...)` from "
        "cmd/server/main.go:129 on the server root context; no HTTP client can "
        "reach it (nothing outside the package calls RunOnce). Its ctx cancelling "
        "IS graceful shutdown."
    ),
    "services/api/internal/pipeline/coordinator.go::fail": (
        "NATS consumer error path. ctx comes from Coordinator.Run via "
        "handleUploaded, i.e. the consumer lifecycle, never a client socket. Its "
        "ctx cancelling IS graceful shutdown."
    ),
    "services/api/internal/pipeline/verify_worker.go::fail": (
        "NATS consumer error path. ctx comes from VerifyWorker.Run via "
        "handleVerification. Same reasoning as coordinator.fail."
    ),
}


WRITE = re.compile(r"\.(Exec|CopyFrom|SendBatch|Commit)\(\s*([A-Za-z_][A-Za-z_0-9.()]*)")
# Contexts that can be a request's. `ctx` is included because most of these
# writes sit in a helper that took the request context as a plain parameter.
REQCTX = re.compile(r"^(r\.Context\(\)|ctx|reqCtx|c\.Context\(\))$")
SWALLOW = re.compile(r"slog\.(Warn|Error)\(")
# A write whose failure reaches the caller is NOT in the class. writeProblem is
# this repo's RFC7807 responder and is as much an escalation as writeJSON — the
# first draft of this sweep omitted it and false-flagged four sites.
ESCALATE = re.compile(r"\b(writeJSON\(w|writeProblem\(|http\.Error\(|"
                      r"return (err|fmt\.Errorf|nil, )|panic\()")
# Matched WITHOUT a leading `^\s*if`, because Go's
#   if _, err := conn.Exec(ctx,\n  "…"); err != nil {
# puts the test on the statement's LAST line. Anchoring skipped past those and
# picked up an unrelated guard further down, which false-flagged AcquireScoped.
ERRGUARD = re.compile(r"err\s*!=\s*nil\s*\{")
FUNCDECL = re.compile(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z_0-9]*)")

SKIP_DIRS = {".git", "node_modules", "target", ".next", "__pycache__", "vendor"}


def code_lines(path):
    """Source with // line comments blanked. ReleaseRLSConn's doc comment QUOTES
    the vulnerable call it replaced (`conn.Exec(r.Context(), "RESET ...")`), and
    three of the four fixes describe the bug they fixed — so a sweep that reads
    comments reports every fix as the bug."""
    out = []
    with open(path, encoding="utf-8") as fh:
        for line in fh.read().split("\n"):
            i = line.find("//")
            out.append(line[:i] if i >= 0 else line)
    return out


def enclosing_func(lines, i):
    """(name, body_start) of the func containing line i, or (None, 0)."""
    for j in range(i, -1, -1):
        m = FUNCDECL.match(lines[j])
        if m:
            return m.group(1), j
    return None, 0


def error_block(lines, i):
    """The body of the `err != nil { … }` belonging to the write at line i, or
    None if there is no guard within 25 lines.

    Reading a fixed 12-line window instead was this sweep's first-draft mistake:
    these writes carry multi-line SQL literals, so the guard that DOES escalate
    sat just past the window and four sites were false-flagged. The block is the
    right unit — it is exactly "what happens when this write fails"."""
    for j in range(i, min(i + 25, len(lines))):
        if ERRGUARD.search(lines[j]):
            depth, k = 0, j
            while k < len(lines):
                depth += lines[k].count("{") - lines[k].count("}")
                if depth <= 0 and k > j:
                    break
                k += 1
            return "\n".join(lines[j:k + 1])
    return None


def go_files(root):
    for base, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for fn in sorted(files):
            if fn.endswith(".go") and not fn.endswith("_test.go"):
                yield os.path.join(base, fn)


def scan(root, rel_to):
    """(examined, flagged, stripped, seen_files, noguard). `stripped` is the set
    of "rel::func" keys where the fix is present; `seen_files` is every rel path
    scanned, so a MUST_STRIP entry whose file has moved is reported as missing
    rather than as a reverted fix."""
    examined, noguard = 0, 0
    flagged, stripped, seen_files = [], set(), set()
    for path in go_files(root):
        rel = os.path.relpath(path, rel_to).replace(os.sep, "/")
        try:
            lines = code_lines(path)
        except (OSError, UnicodeDecodeError):
            continue
        seen_files.add(rel)
        for i, line in enumerate(lines):
            m = WRITE.search(line)
            if not m or not REQCTX.match(m.group(2)):
                continue
            examined += 1
            name, start = enclosing_func(lines, i)
            key = "%s::%s" % (rel, name)
            # Already fixed? Skip it — a guard that re-flags its own fixes can
            # never go green and is useless in CI.
            if "context.WithoutCancel" in "\n".join(lines[start:i]):
                stripped.add(key)
                continue
            block = error_block(lines, i)
            if block is None:
                noguard += 1
                continue
            if SWALLOW.search(block) and not ESCALATE.search(block):
                flagged.append((key, i + 1, m.group(1), m.group(2)))
    return examined, flagged, stripped, seen_files, noguard


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root", nargs="?", default=os.path.join(ROOT, "services"))
    # Keys are relative paths, so a mutant run against a COPY of the tree must
    # be told where that copy's root is — otherwise every key comes out as
    # "../../tmp/..." , no table entry matches, and the guard blanket-fails,
    # which would make a mutant test prove nothing.
    ap.add_argument("--rel-to", default=ROOT)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    examined, flagged, stripped, seen_files, noguard = scan(args.root, args.rel_to)
    print("%d request-context write(s) examined; %d already strip cancellation; "
          "%d had no `err != nil` guard within 25 lines and were skipped"
          % (examined, len(stripped), noguard))

    failed = False

    # Inertness. If the write pattern stops matching, the negative half below
    # reports a clean tree for the wrong reason.
    if examined == 0:
        print("\nFAIL: found no request-context database write at all — the "
              "pattern no longer matches this codebase and this guard is inert.")
        failed = True

    # Positive half: every known fix must still be in place.
    missing = [k for k in MUST_STRIP if k.split("::")[0] not in seen_files]
    reverted = [k for k in MUST_STRIP if k not in stripped and k not in missing]
    if reverted:
        print("\nREVERTED FIX — these must strip cancellation and no longer do:")
        for k in sorted(reverted):
            print("   %s\n      why it matters: %s" % (k, MUST_STRIP[k]))
        failed = True
    if missing:
        print("\nMISSING FILE — a MUST_STRIP entry points at a file not in the "
              "scanned tree; it moved, or this scan is not covering it:")
        for k in sorted(missing):
            print("   %s" % k)
        failed = True
    if not reverted and not missing and args.verbose:
        for k in sorted(MUST_STRIP):
            print("  fix in place: %s" % k)


    # Negative half: no unaccounted candidates.
    unaccounted = [f for f in flagged if f[0] not in ALLOWLIST]
    if args.verbose:
        for k, why in sorted(ALLOWLIST.items()):
            seen = " (matched)" if any(f[0] == k for f in flagged) else \
                   " (NOT MATCHED — entry may be stale)"
            print("  allowlisted: %s%s\n      reason: %s" % (k, seen, why))
    if unaccounted:
        print("\n%d candidate(s) where losing the write is SILENT:" % len(unaccounted))
        for key, ln, meth, ctxname in unaccounted:
            print("   %s (line %d): .%s(%s) — its `err != nil` guard logs and "
                  "continues" % (key, ln, meth, ctxname))
        print("\nJudge each: is this write the only record of a security or audit "
              "fact, and can the party being recorded cancel it? If yes, strip "
              "cancellation (context.WithoutCancel + a bounded deadline). If no, "
              "add it to ALLOWLIST in this script with the reason. Do not remove "
              "this check.")
        failed = True

    if failed:
        return 1
    print("OK: no unaccounted cancellable audit writes; all %d known fixes in "
          "place." % len(MUST_STRIP))
    return 0


if __name__ == "__main__":
    sys.exit(main())

