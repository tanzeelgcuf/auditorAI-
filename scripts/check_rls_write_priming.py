#!/usr/bin/env python3
"""Every statement against an RLS table must run on an RLS-primed connection.

THE BUG CLASS. Policies in infra/init.sql call current_setting('app.current_firm')
or current_setting('app.assigned_books') with NO missing_ok argument and cast the
result straight to uuid / uuid[]. On a connection that RLSInjector never primed,
such a predicate does not evaluate to false — it raises: 42704 undefined_object
if the GUC was never set on that physical connection, or 22P02
invalid_text_representation on ''::uuid once middleware.ReleaseRLSConn has RESET
it. So a statement on the raw *pgxpool.Pool cannot work against these tables.

THE SHAPE THAT HID IT. Four handlers wrote `c := middleware.GetConn(ctx); if c ==
nil { c, _ = s.db.Acquire(ctx) }`. Acquire() succeeds, so the branch LOOKS
handled — but the connection it returns is unprimed, which is the one thing the
nil check was there to prevent. Fabricating a substitute for a value you could
not resolve is worse than failing, because the failure moves to a place that
cannot explain it.

WHAT THIS GUARD DOES. Negative half: extracts the RLS table set from init.sql,
finds every .Exec/.Query/.QueryRow/.Begin call in the Go tree whose SQL touches
one of those tables, resolves the receiver, and fails on any receiver that is a
bare pool not accounted for in ALLOWLIST. Positive half: asserts each fixed site
still carries its fix, keyed by name, so reverting one fails even if the search
patterns stop matching. Inertness half: if the parse yields no RLS tables, no
statements, or no already-accepted receivers, the guard failed to parse the tree
and exits non-zero rather than passing vacuously.

Exit 0 = clean. Exit 1 = a finding. Exit 2 = the guard could not parse.
"""

import argparse
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
INIT_SQL = os.path.join(REPO, "infra", "init.sql")
GO_ROOT = os.path.join(REPO, "services", "api")

# Receivers that ARE primed, or that resolve to a primed connection.
#   middleware.DB(ctx, x) / DB(ctx, x)  the house helper: returns the primed
#       request connection when there is one. Still falls through to the pool on
#       an unprimed request, which is a documented residual (see DB()'s own
#       comment) — accepted here because it is the sanctioned form and changing
#       it is a whole-tree signature change, not a fix this guard can demand.
#   tx                                   inherits priming from its Begin receiver.
#   conn / c / db / driver / q           only when the enclosing function resolved
#       them from GetConn / s.primed / s.conn — checked, not assumed.
PRIMED_CALL = re.compile(r"^(?:middleware\.)?DB\(")
RESOLVERS = (
    "middleware.GetConn(",
    "s.primed(",
    "s.conn(",
    "= conn(r)",          # periods.go's package-level wrapper over GetConn
    "getConn(",           # portal.go's reader of the AcquireScoped connection
    "AcquireScoped(",
)

# A bare pool reached through a struct field or a *pgxpool.Pool parameter. Field
# selectors are unconditional; bare identifiers are NOT, because `db :=
# middleware.GetConn(...)` is a primed connection called `db` and classifying it
# on the name alone reports the fix as the bug (it did, on the first run).
BARE_FIELD = re.compile(r"^[A-Za-z_]\w*\.(?:db|pool|sysDB|sysdb)$")
BARE_IDENT = re.compile(r"^(?:db|pool|sysDB|conn|c|driver|q)$")


SQL_CALL = re.compile(r"([A-Za-z_][A-Za-z0-9_.]*(?:\([^()]*\))?)\.(Exec|Query|QueryRow|Begin)\(")

# FABRICATION HALF. `<pool>.Acquire(` is the anti-pattern spelled out literally,
# and catching it by SHAPE is strictly stronger than inferring from the receiver
# of the eventual statement. Mutant M1 proved why this half is needed: reverting
# HandleListBooks to `c := GetConn(...); if c == nil { c, _ = s.db.Acquire(...) }`
# was caught ONLY by that file's pin, because the enclosing function still
# contains `middleware.GetConn(` and RESOLVERS therefore classified the receiver
# as primed. In a file with no pin, that shape was invisible — and it was: this
# detector, added after M1, immediately found five unflagged instances (mcp.go
# x4, review.go x1) that three prior sweeps of this bug class had missed.
ACQUIRE_PAT = re.compile(r"\b([A-Za-z_][A-Za-z0-9_.]*)\.Acquire\(")

# Acquire() calls that are CORRECT. Keyed file -> {receiver: reason}. Anything
# else is a fabricated connection: Acquire() hands back a pooled connection with
# no app.current_firm set, so it satisfies a nil check while being the one thing
# the nil check existed to prevent.
ACQUIRE_ALLOW = {
    "internal/middleware/middleware.go": {
        "db": "RLSInjector:128 — acquires the connection it is about to prime.",
        "pool": "AcquireScoped:185 — same, for the portal and internal-auth paths.",
    },
    "internal/auth/auth.go": {
        "s.db": "authSvc.SetDB(sysPool) main.go:155. sysPool is BYPASSRLS and every "
                "/v1/auth route is pre-scope, so there are no GUCs to set and "
                "nothing to prime — an unprimed sys connection is the correct one.",
    },
}


# INSERT INTO x / UPDATE x / DELETE FROM x / FROM x / JOIN x
TABLE_PAT = re.compile(
    r"\b(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM|FROM|JOIN)\s+([a-z_][a-z0-9_]*)",
    re.I,
)
WRITE_PAT = re.compile(r"\b(?:INSERT|UPDATE|DELETE)\b", re.I)

# Bare-pool statements that are CORRECT because the pool is sysPool (auditor_sys,
# BYPASSRLS) or because no request is behind them. Each entry is file:symbol and
# each reason names the main.go line that assigns the pool — resolve the pool
# before calling a raw write a bug. cmd/server/main.go documents its own reasoning
# inline; this table mirrors it so the guard can be read without it.
# Bare-pool statements that are CORRECT because the receiver is sysPool
# (auditor_sys, BYPASSRLS) or because no request is behind them. Keyed by
# file -> {receiver: reason}. Deliberately per-RECEIVER, not per-file: billing.go
# holds BOTH s.sysDB (sys) and s.db (app), and portal.go holds s.sysDB alongside
# app-pool work, so a file-level pass would excuse exactly the writes this guard
# exists to catch. Each reason names the main.go line that assigns the pool —
# resolve the pool before calling a raw write a bug.
ALLOWLIST = {
    "internal/auth/auth.go": {"s.db":
        "authSvc.SetDB(sysPool) main.go:155 — every /v1/auth route is mounted "
        "PUBLIC, before any GUC exists."},
    "internal/webhooks/webhooks.go": {"s.db":
        "webhooksSvc.SetDB(sysPool) main.go:248 — keeps its own WHERE firm_id = $1."},
    "internal/billing/billing.go": {"s.sysDB":
        "billingSvc.SetSysDB(sysPool) main.go:235 — a Stripe webhook has no firm "
        "in context; without BYPASSRLS the UPDATE would raise rather than match "
        "zero rows. s.db in this same file is the APP pool and is NOT excused."},
    "internal/pipeline/coordinator.go": {"c.db":
        "pipeline.NewCoordinator(…, sysPool, st) main.go:177 — background worker."},
    "internal/pipeline/verify_worker.go": {"w.db":
        "NewVerifyWorker(…, sysPool) main.go:196 — background worker."},
    "internal/notify/notify.go": {"db":
        "notify.Run(ctx, sysPool, …) main.go:129 — a sweep across all firms "
        "cannot be scoped to one."},
    "internal/middleware/internal_auth.go": {"sysDB":
        "middleware.InternalAuth(pool, sysPool) main.go:511 — the sysDB parameter "
        "is sysPool; it resolves book -> firm, the step that ESTABLISHES scope, so "
        "it cannot itself be scoped."},
    "internal/portal/portal.go": {"s.sysDB":
        "portalSvc.SetSysDB(sysPool) main.go:255 — the pre-auth invite lookup and "
        "the book -> firm resolution run before any firm is known (portal.go:34-37)."},
    "internal/settings/settings.go": {"sysDB":
        "settings.AuthAPIKey's sysDB PARAMETER, not a service field. The function "
        "discovers which firm a presented key belongs to, so there cannot be a "
        "primed connection yet — same pre-scope position as authSvc. Currently "
        "unwired (zero callers); the parameter name is the contract. Note this "
        "excuses the receiver `sysDB` in this file only, so renaming an app-pool "
        "variable to sysDB elsewhere still fails."},
    "internal/push/push.go": {"sysDB":
        "push.SendFindingAlert's sysDB PARAMETER. Dead code today; its only "
        "possible caller is a background goroutine with no request, so there is no "
        "primed connection to use and no request connection to fall back to."},
    "internal/middleware/middleware.go": {"*":
        "RLSInjector and AcquireScoped are what DO the priming; they must reach "
        "the pool directly."},
    "cmd/seed-demo/main.go": {"*":
        "takes -db-url and REFUSES the auditor_app DSN at runtime "
        "(main.go:69,108-109)."},
}


# POSITIVE HALF. Each fixed site, pinned by name. A guard that only detects NEW
# instances lets the fixed instance rot back: reverting one of these passes every
# pattern-based check in this repo, which is not hypothetical — it is why this
# half exists. `must` = substrings that must be present; `must_not` = substrings
# whose reappearance IS the reintroduction.
PINS = {
    "internal/tenant/tenant.go": {
        "must": ["func (s *Service) primed(", "s.primed(w, r)"],
        "must_not": ["s.db.Acquire(", "s.db.Query(", "s.db.QueryRow("],
        "why": "four handlers (CreateBook, ListBooks, GetBook, UpdateBookSettings) "
               "each hand-rolled GetConn-or-Acquire; now one resolution point.",
    },
    "internal/tenant/rotate_keys.go": {
        "must": ["s.primed(w, r)"],
        "must_not": ["s.db.Acquire("],
        "why": "data_encryption_keys write; the fallback would raise on "
               "dek_firm_isolation.",
    },
    "internal/settings/settings.go": {
        "must": ["ErrNoPrimedConn", "return nil, nil, ErrNoPrimedConn"],
        "must_not": ["s.db.Acquire("],
        "why": "conn() fed ~10 sites across chart_of_accounts, "
               "counterparty_aliases, csv_column_mappings, api_keys, "
               "webhook_subscriptions — all RLS.",
    },
    "internal/push/push.go": {
        "must": ["db := middleware.GetConn(r.Context())", "if db == nil {"],
        "must_not": ["s.db.Acquire("],
        "why": "device_tokens write on an unprimed connection.",
    },
    "internal/middleware/idempotency.go": {
        "must": ["DB(ctx, db).QueryRow(", "DB(cctx, db).Exec(", "idemCtxKey{}"],
        "must_not": ["db.QueryRow(ctx,", "hashKey(userID"],
        "why": "idempotency_keys gained RLS on 2026-09-05; both statements had to "
               "move to the primed connection FIRST, and the cache key is resolved "
               "in exactly one place.",
    },
    "internal/humanoverride/humanoverride.go": {
        "must": ["middleware.DB(ctx, db).Exec("],
        "must_not": [],
        "why": "config_change_log write — was on the raw pool, so the table was "
               "very likely always empty.",
    },
    "internal/mcp/mcp.go": {
        "must": ["func (s *Service) primed(", "c, ok := s.primed(w, r)"],
        "must_not": ["s.db.Acquire(", "func (s *Service) SetDB("],
        "why": "all four MCP tools had the identical GetConn-or-Acquire block on the "
               "RLS-enforced pool (main.go:241). Found 2026-09-05 by the fabrication "
               "half, after the receiver-based half had passed this file — its "
               "verdict was 'primed' both before and after the fix, because "
               "middleware.GetConn( is in scope either way.",
    },
    "internal/review/review.go": {
        "must": ["func (s *Service) primed(", "c, ok := s.primed(w, r)"],
        "must_not": ["s.db.Acquire("],
        "why": "review-queue read on reconciliation_groups; same shape, same pool "
               "(main.go:219). The pool field legitimately remains for RecordAccess.",
    },
}


def rls_tables(path):
    """Tables with ENABLE ROW LEVEL SECURITY. FORCE is applied by a DO block over
    pg_class (init.sql ~:920), so grepping for the literal FORCE statement finds
    nothing — ENABLE is the parseable signal."""
    with open(path, encoding="utf-8") as fh:
        sql = fh.read()
    return set(
        m.lower()
        for m in re.findall(
            r"ALTER\s+TABLE\s+([a-z_][a-z0-9_]*)\s+ENABLE\s+ROW\s+LEVEL\s+SECURITY",
            sql, re.I)
    )


def strip_line_comments(src):
    """Remove // comments without truncating on a // that lives inside a string
    literal. Getting this order wrong is what made an earlier ad-hoc checker
    report unbalanced parens in three files that were fine."""
    out, i, n = [], 0, len(src)
    while i < n:
        ch = src[i]
        if ch in "\"'`":
            q, j = ch, i + 1
            while j < n:
                if src[j] == "\\" and q != "`":
                    j += 2
                    continue
                if src[j] == q:
                    j += 1
                    break
                j += 1
            out.append(src[i:j])
            i = j
            continue
        if ch == "/" and i + 1 < n and src[i + 1] == "/":
            while i < n and src[i] != "\n":
                i += 1
            continue
        if ch == "/" and i + 1 < n and src[i + 1] == "*":
            k = src.find("*/", i)
            i = n if k < 0 else k + 2
            continue
        out.append(ch)
        i += 1
    return "".join(out)

def sql_after(src, pos):
    """The first string literal in the argument list starting at pos. Handles
    backtick-quoted multi-line SQL and plain double-quoted one-liners."""
    depth, i, n = 0, pos, len(src)
    while i < n:
        ch = src[i]
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth <= 0:
                return ""
        elif ch == "`":
            j = src.find("`", i + 1)
            return src[i + 1:j] if j > 0 else ""
        elif ch == '"':
            j = i + 1
            while j < n and src[j] != '"':
                j += 2 if src[j] == "\\" else 1
            return src[i + 1:j]
        i += 1
    return ""


def resolver_scope(src, call_pos):
    """Text of the enclosing function, used to see whether `conn`/`db`/`tx` was
    resolved from a primed source. Walks back to the previous top-level `func `,
    which is sufficient because Go forbids nested named functions at column 0."""
    start = src.rfind("\nfunc ", 0, call_pos)
    if start < 0:
        start = 0
    end = src.find("\nfunc ", call_pos)
    if end < 0:
        end = len(src)
    return src[start:end]

def scan_acquires():
    """Yield (relkey, line, receiver) for every .Acquire( that is not accounted
    for. Comments are stripped first: four files in this tree describe the old
    `s.db.Acquire()` bug in prose, and matching prose would report the fixes as
    the defect (it did, for the pins, before they were comment-stripped)."""
    out, total = [], 0
    for root, dirs, files in os.walk(GO_ROOT):
        dirs[:] = [d for d in dirs if d not in (".git", "vendor", "testdata")]
        for fn in sorted(files):
            if not fn.endswith(".go") or fn.endswith("_test.go"):
                continue
            full = os.path.join(root, fn)
            relkey = os.path.relpath(full, GO_ROOT).replace(os.sep, "/")
            with open(full, encoding="utf-8") as fh:
                src = strip_line_comments(fh.read())
            for m in ACQUIRE_PAT.finditer(src):
                recv = m.group(1)
                total += 1
                if ACQUIRE_ALLOW.get(relkey, {}).get(recv):
                    continue
                out.append((relkey, src[:m.start()].count("\n") + 1, recv))
    return out, total


def scan(tables):
    """Yield (relkey, line, receiver, method, kind, hit_tables, verdict)."""
    found = []
    for root, dirs, files in os.walk(GO_ROOT):
        dirs[:] = [d for d in dirs if d not in (".git", "vendor", "testdata")]
        for fn in sorted(files):
            if not fn.endswith(".go") or fn.endswith("_test.go"):
                continue
            full = os.path.join(root, fn)
            relkey = os.path.relpath(full, GO_ROOT).replace(os.sep, "/")
            with open(full, encoding="utf-8") as fh:
                raw = fh.read()
            src = strip_line_comments(raw)
            for m in SQL_CALL.finditer(src):
                recv, meth = m.group(1), m.group(2)
                sql = sql_after(src, m.end() - 1)
                hit = sorted({t.lower() for t in TABLE_PAT.findall(sql)} & tables)
                if not hit:
                    continue
                kind = "write" if WRITE_PAT.search(sql) else "read"
                line = src[:m.start()].count("\n") + 1
                verdict = classify(recv, resolver_scope(src, m.start()))
                found.append((relkey, line, recv, meth, kind, hit, verdict))
    return found


def classify(recv, scope):
    if PRIMED_CALL.match(recv):
        return "primed"
    if recv == "tx" or recv.split(".")[0] == "tx":
        return "primed"
    if BARE_FIELD.match(recv):
        return "bare-pool"
    if BARE_IDENT.match(recv):
        # Resolve, do not guess from the name.
        if any(rsv in scope for rsv in RESOLVERS):
            return "primed"
        return "bare-pool"
    return "other"


def check_pins(verbose):
    """Positive half. Returns (failures, checked_count)."""
    fails, checked = [], 0
    for relkey, spec in sorted(PINS.items()):
        full = os.path.join(GO_ROOT, relkey)
        if not os.path.exists(full):
            fails.append("MISSING FILE %s — a pin points at a file not in the "
                         "tree; it moved, or this scan is not covering it." % relkey)
            continue
        with open(full, encoding="utf-8") as fh:
            body = strip_line_comments(fh.read())
        checked += 1
        for needle in spec["must"]:
            if needle not in body:
                fails.append("REVERTED %s — lost %r\n      why it matters: %s"
                             % (relkey, needle, spec["why"]))
        for needle in spec["must_not"]:
            if needle in body:
                fails.append("REINTRODUCED %s — %r is back\n      why it matters: %s"
                             % (relkey, needle, spec["why"]))
        if verbose:
            print("  pin ok: %s" % relkey)
    return fails, checked


def allow_reason(relkey, recv):
    spec = ALLOWLIST.get(relkey)
    if not spec:
        return None
    return spec.get("*") or spec.get(recv)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    if not os.path.exists(INIT_SQL):
        print("GUARD CANNOT RUN: %s not found" % INIT_SQL)
        return 2
    tables = rls_tables(INIT_SQL)
    found = scan(tables)
    acq, acq_total = scan_acquires()

    # INERTNESS HALF. Every one of these is a way for the guard to pass while
    # having checked nothing, which is the failure mode that matters most: a green
    # guard is read as evidence. Zero matches must FAIL, never quietly succeed.
    primed = [f for f in found if f[6] == "primed"]
    if len(tables) < 20:
        print("GUARD IS INERT: parsed only %d RLS tables from init.sql (expected "
              "~30). The ALTER TABLE … ENABLE ROW LEVEL SECURITY form changed, or "
              "this is not the schema." % len(tables))
        return 2
    if len(found) < 40:
        print("GUARD IS INERT: matched only %d statements against RLS tables "
              "(expected 70+). The .Exec/.Query call shape or the SQL literal "
              "form changed and this guard is no longer reading the tree."
              % len(found))
        return 2
    if not primed:
        print("GUARD IS INERT: not one statement resolved to a primed connection. "
              "Receiver classification is broken, so every site would look "
              "equally bad and the negative half would be meaningless.")
        return 2
    if acq_total == 0:
        print("GUARD IS INERT: zero .Acquire( calls found in the tree. There are "
              "at least three legitimate ones (RLSInjector, AcquireScoped, "
              "authSvc), so the fabrication half is not reading the tree.")
        return 2

    failed = False
    pin_fails, pin_count = check_pins(args.verbose)
    if pin_fails:
        print("\nPOSITIVE HALF FAILED — a fix was reverted or reintroduced:")
        for f in pin_fails:
            print("   %s" % f)
        failed = True

    # FABRICATION HALF.
    if acq:
        print("\n%d unaccounted .Acquire( call(s) — a connection fabricated where a "
              "primed one was required:" % len(acq))
        for relkey, line, recv in acq:
            print("   %s:%d  %s.Acquire(...)" % (relkey, line, recv))
        print("\nAcquire() returns a pooled connection with NO app.current_firm set, "
              "so `if c == nil { c, _ = pool.Acquire(ctx) }` satisfies the nil check "
              "with the exact value the nil check existed to reject. Every policy in "
              "init.sql casts current_setting(...) straight to uuid with no "
              "missing_ok, so such a statement RAISES (42704, or 22P02 on ''::uuid "
              "after ReleaseRLSConn RESETs it) — it does not filter and does not "
              "return zero rows. Fail closed instead: log, 500, return. If the "
              "receiver is the BYPASSRLS sysPool or is the code doing the priming, "
              "add it to ACQUIRE_ALLOW with the main.go line that assigns it.")
        failed = True

    # NEGATIVE HALF.
    bad = []
    for relkey, line, recv, meth, kind, hit, verdict in found:
        if verdict == "primed":
            continue
        if allow_reason(relkey, recv):
            continue
        bad.append((relkey, line, recv, meth, kind, hit, verdict))

    if args.verbose:
        print("\n  %d RLS tables, %d statements touching them, %d on a primed "
              "receiver, %d .Acquire( calls (%d accounted for), %d pins checked"
              % (len(tables), len(found), len(primed), acq_total,
                 acq_total - len(acq), pin_count))
        for relkey, spec in sorted(ALLOWLIST.items()):
            for recv, why in sorted(spec.items()):
                seen = any(f[0] == relkey and (recv == "*" or f[2] == recv)
                           for f in found)
                print("  allowlisted: %s [%s]%s\n      reason: %s"
                      % (relkey, recv,
                         "" if seen else "  (NOT MATCHED — entry may be stale)", why))

    if bad:
        print("\n%d statement(s) against an RLS table on a connection that is not "
              "known to be primed:" % len(bad))
        for relkey, line, recv, meth, kind, hit, verdict in bad:
            print("   %s:%d  %s.%s(...)  [%s on %s]  receiver=%s"
                  % (relkey, line, recv, meth, kind, ",".join(hit), verdict))
        print("\nJudge each. If the receiver is the BYPASSRLS sysPool, add the file "
              "to ALLOWLIST with the main.go line that assigns it. If it is the "
              "app pool, route the statement through middleware.DB(ctx, db) or "
              "resolve a primed connection and FAIL CLOSED when there is none — do "
              "NOT substitute pool.Acquire(), which returns an unprimed connection "
              "and moves the failure somewhere that cannot explain it. Do not "
              "remove this check.")
        failed = True

    if failed:
        return 1
    print("OK: %d statements against %d RLS tables, none on an unaccounted bare "
          "pool; %d .Acquire( calls, all accounted for; all %d pinned fixes in "
          "place." % (len(found), len(tables), acq_total, pin_count))

    return 0


if __name__ == "__main__":
    sys.exit(main())
