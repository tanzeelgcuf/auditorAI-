#!/usr/bin/env python3
"""Mutant-test scripts/check_rls_write_priming.py.

A guard that has never been shown to fail is not evidence. For each fixed site,
copy the working tree, reintroduce the ORIGINAL defect in that one place, and
require the guard to (a) exit non-zero and (b) NAME that site. A mutant that
trips a different pin is a FAIL, not a pass — attribution is the whole point.

Each mutant starts from a pristine copy, so mutation N cannot be contaminated
by residue from mutation N-1.
"""

import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_TMP = tempfile.mkdtemp(prefix="rlsmut-")
BASE = os.path.join(_TMP, "base")
WORK = os.path.join(_TMP, "work")
GUARD = "scripts/check_rls_write_priming.py"
# Only the subtrees the guard reads. Copying the whole repo would drag in .git
# and node_modules for no benefit.
SUBTREES = ("scripts", "infra", "services/api")


def build_base():
    """Snapshot the live tree once. Every mutant is a fresh copy OF THIS, so the
    baseline is measured on the same bytes the mutants start from — otherwise a
    'caught' result could be attributable to an unrelated difference."""
    for sub in SUBTREES:
        src = os.path.join(REPO, sub)
        if not os.path.isdir(src):
            print("ABORT: %s not found under %s" % (sub, REPO))
            sys.exit(2)
        shutil.copytree(src, os.path.join(BASE, sub),
                        ignore=shutil.ignore_patterns(".git", "node_modules",
                                                      "target", "__pycache__"))

# (id, relpath-under-tree, old, new, expected substring in guard output, note)
MUTANTS = [
    ("M1-tenant-listbooks",
     "services/api/internal/tenant/tenant.go",
     "\tconn, ok := s.primed(w, r)\n\tif !ok {\n\t\treturn\n\t}",
     "\tconn := middleware.GetConn(r.Context())\n\tif conn == nil {\n\t\tconn, _ = s.db.Acquire(r.Context())\n\t}",
     "internal/tenant/tenant.go",
     "HandleListBooks reverts to GetConn-or-Acquire"),

    ("M2-rotate-keys",
     "services/api/internal/tenant/rotate_keys.go",
     "\tdb, ok := s.primed(w, r)\n\tif !ok {\n\t\treturn\n\t}",
     "\tdb := middleware.GetConn(r.Context())\n\tif db == nil {\n\t\tdb, _ = s.db.Acquire(r.Context())\n\t}",
     "internal/tenant/rotate_keys.go",
     "data_encryption_keys write back on an unprimed connection"),

    ("M3-settings-conn",
     "services/api/internal/settings/settings.go",
     "return nil, nil, ErrNoPrimedConn",
     "c, err := s.db.Acquire(ctx)\n\t\tif err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\treturn c, func() { c.Release() }, nil",
     "internal/settings/settings.go",
     "conn() fabricates an unprimed connection again (~10 downstream sites)"),

    ("M4-push-register",
     "services/api/internal/push/push.go",
     "\tdb := middleware.GetConn(r.Context())\n\tif db == nil {",
     "\tdb := middleware.GetConn(r.Context())\n\tif false {",
     "internal/push/push.go",
     "device_tokens write with the fail-closed branch made unreachable"),

    ("M5-idempotency",
     "services/api/internal/middleware/idempotency.go",
     "DB(ctx, db).QueryRow(",
     "db.QueryRow(ctx,",
     "internal/middleware/idempotency.go",
     "idempotency_keys read leaves the primed connection"),

    ("M6-humanoverride",
     "services/api/internal/humanoverride/humanoverride.go",
     "middleware.DB(ctx, db).Exec(",
     "db.Exec(ctx,",
     "internal/humanoverride/humanoverride.go",
     "config_change_log write back on the raw pool"),

    ("M7-mcp-tools",
     "services/api/internal/mcp/mcp.go",
     "\tc, ok := s.primed(w, r)\n\tif !ok {\n\t\treturn\n\t}",
     "\tc := middleware.GetConn(r.Context())\n\tif c == nil {\n\t\tc2, err := s.db.Acquire(r.Context())\n\t\tif err != nil {\n\t\t\treturn\n\t\t}\n\t\tdefer c2.Release()\n\t\tc = c2\n\t}",
     "internal/mcp/mcp.go",
     "one MCP tool handler back to GetConn-or-Acquire on the app pool"),

    ("M8-review-queue",
     "services/api/internal/review/review.go",
     "\tc, ok := s.primed(w, r)\n\tif !ok {\n\t\treturn\n\t}",
     "\tc := middleware.GetConn(r.Context())\n\tif c == nil {\n\t\tc2, err := s.db.Acquire(r.Context())\n\t\tif err != nil {\n\t\t\treturn\n\t\t}\n\t\tdefer c2.Release()\n\t\tc = c2\n\t}",
     "internal/review/review.go",
     "review-queue read back to GetConn-or-Acquire on the app pool"),

    # A NEW site, in a file with no pin at all. This is the one mutant the
    # positive half cannot possibly catch, so it isolates the fabrication half:
    # if this is missed, the guard only protects code it already knows about.
    ("M9-new-unpinned-site",
     "services/api/internal/findings/findings.go",
     "func writeJSON(",
     "func fabricate(s *Service, ctx context.Context) {\n"
     "\tc, _ := s.db.Acquire(ctx)\n"
     "\tdefer c.Release()\n"
     "\tc.Exec(ctx, `UPDATE client_books SET client_name = 'x'`)\n"
     "}\n\nfunc writeJSON(",
     "internal/findings/findings.go",
     "a brand-new Acquire() fabrication in a file the guard has no pin for"),
]

# INERTNESS MUTANTS. Not string edits — these break the guard's INPUT, and the
# requirement is exit 2 (cannot parse) rather than exit 1 (finding). A guard that
# answers "OK" when it parsed nothing is worse than no guard, because the OK gets
# quoted as evidence.
INERT = [
    ("I1-no-rls-tables",
     "drop every ALTER TABLE ... ENABLE ROW LEVEL SECURITY from init.sql",
     lambda tree: _sub_file(os.path.join(tree, "infra/init.sql"),
                            "ENABLE ROW LEVEL SECURITY", "ENABLE TRIGGER ALL"),
     "parsed only 0 RLS tables"),

    ("I2-no-statements",
     "remove every .go file the scanner walks",
     lambda tree: _rm_go(os.path.join(tree, "services/api")),
     "matched only 0 statements"),
]


def _sub_file(path, old, new):
    with open(path, encoding="utf-8") as fh:
        body = fh.read()
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(body.replace(old, new))


def _rm_go(root):
    for r, _, files in os.walk(root):
        for fn in files:
            if fn.endswith(".go"):
                os.remove(os.path.join(r, fn))


def fresh():
    if os.path.isdir(WORK):
        shutil.rmtree(WORK)
    shutil.copytree(BASE, WORK)


def run_guard(tree=WORK):
    p = subprocess.run([sys.executable, os.path.join(tree, GUARD)],
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def main():
    build_base()
    rc, out = run_guard(BASE)
    print("=== BASELINE (unmutated scratch tree) ===")
    print("exit=%d  %s" % (rc, out.strip().splitlines()[-1] if out.strip() else ""))
    if rc != 0:
        print("ABORT: baseline is not clean, so no mutant result is attributable.")
        return 2

    results = []
    for mid, rel, old, new, expect, note in MUTANTS:
        fresh()
        path = os.path.join(WORK, rel)
        with open(path, encoding="utf-8") as fh:
            body = fh.read()
        n = body.count(old)
        if n == 0:
            results.append((mid, "NO-OP", 0, "mutation string absent — pin may be "
                            "pointing at code that no longer exists", note))
            continue
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(body.replace(old, new, 1))
        rc, out = run_guard()
        named = expect in out
        # Attribution: no OTHER pinned file may be named.
        others = [o for _, orel, _, _, o, _ in MUTANTS
                  if o != expect and o in out]
        if rc != 0 and named and not others:
            verdict = "CAUGHT"
        elif rc != 0 and named and others:
            verdict = "CAUGHT-BUT-NOISY"
        elif rc != 0:
            verdict = "WRONG-SITE"
        else:
            verdict = "MISSED"
        detail = " | ".join(l.strip() for l in out.strip().splitlines()
                            if expect in l or "REVERTED" in l or "REINTRODUCED" in l)
        results.append((mid, verdict, rc, detail[:400], note))

    print("\n=== MUTANTS ===")
    bad = 0
    for mid, verdict, rc, detail, note in results:
        flag = "" if verdict == "CAUGHT" else "   <<< PROBLEM"
        if verdict != "CAUGHT":
            bad += 1
        print("\n%-22s %-16s exit=%d%s\n  reintroduced: %s\n  guard said:   %s"
              % (mid, verdict, rc, flag, note, detail or "(nothing)"))

    print("\n%d/%d mutants caught and correctly attributed."
          % (len(results) - bad, len(results)))

    print("\n=== INERTNESS (must exit 2, not 0) ===")
    for iid, desc, breaker, expect in INERT:
        fresh()
        breaker(WORK)
        rc, out = run_guard()
        ok = rc == 2 and expect in out
        if not ok:
            bad += 1
        print("\n%-18s %s exit=%d%s\n  broke:      %s\n  guard said: %s"
              % (iid, "REFUSED" if ok else "DID NOT REFUSE", rc,
                 "" if ok else "   <<< PROBLEM", desc,
                 out.strip().splitlines()[0][:200] if out.strip() else "(nothing)"))

    return 1 if bad else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        shutil.rmtree(_TMP, ignore_errors=True)

