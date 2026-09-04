#!/usr/bin/env python3
"""Guard: the audit tables record a source IP, and every bind list still lines up.

WHY THIS EXISTS. pgx binds AND scans BY POSITION. Adding `source_ip` to the three
audit INSERTs meant adding a column name, a `$n` placeholder and a Go argument in
three places each, and the two ways to get that wrong fail very differently:

  * column count > arg count  -> runtime error, degraded to slog.Warn by
    RecordAccess and LogConfigChange, so the audit row silently vanishes;
  * right count, wrong ORDER  -> no error at all. `action` lands in `resource_id`
    and the audit trail is confidently wrong, which for this product is worse
    than empty.

The same hazard on the read side is worse still: HandleConfigHistory `continue`s
on a Scan error inside `for rows.Next()`, so a select list one item longer than
its Scan list returns `{"items": []}` for every book, forever, with a 200.

So this guard checks three things, and the third is the one that survives regex
drift: (1) for each audit write, column count == placeholder count == Go arg
count; (2) for the one audit reader, select-list length == Scan-target length;
(3) BY NAME, that `source_ip` is still written by each of the three sites and
still read by the reader. If (1) and (2) stop matching this codebase they go
quiet; (3) fails loudly.

It also pins the middleware ORDER. `middleware.SourceIP` copies the already
rewritten `r.RemoteAddr` onto the context, so mounted above `middleware.RealIP`
it records the PROXY's address rather than the client's on every request behind a
trusted proxy — no error, no log line, a permanently and invisibly wrong audit
trail. That is a source-order property no unit test on either function can see.

WHAT THIS CANNOT DO. It reads source text. It does not connect to Postgres, so it
cannot tell you the column exists in a deployed database (`check_schema_drift.py`
covers init.sql agreement) and it cannot prove a row was ever written. Those need
the DATABASE_URL_TEST suite.

Exit 0 = every arity agrees, source_ip present at all four sites, order correct.
Exit 1 = an arity mismatch, a missing source_ip, a bad order, or an inert guard.
"""

import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Audit writes that MUST carry source_ip. Keyed "relpath::table"; the reason is
# recorded so a future reader knows what removing it would cost.
WRITES = {
    "services/api/internal/middleware/auditlog.go::access_log": (
        "The only writer of access_log, which is the audit trail this product "
        "sells. Until 2026-09-05 it could not answer 'from where'."
    ),
    "services/api/internal/humanoverride/humanoverride.go::config_change_log": (
        "Records who changed a book's tolerances. Without the address, a "
        "disputed threshold change cannot be tied to a session."
    ),
    "services/api/internal/periods/periods.go::period_reopen_log": (
        "Reopening a closed period is the one action that lets an already "
        "signed-off number change. Highest-value 'from where' in the product."
    ),
}

# Audit readers whose select list and Scan list must stay the same length.
READS = {
    "services/api/internal/humanoverride/humanoverride.go::config_change_log": (
        "GET /v1/books/{bookId}/config-history. Scans positionally and skips "
        "rows on Scan error, so a length mismatch returns an empty list with 200."
    ),
}

MAIN_GO = "services/api/cmd/server/main.go"

# Audit writes that must go through the RLS-WIRED REQUEST CONNECTION, keyed
# "relpath::func" with the call expression that has to be present.
#
# This half is positive-only, and it is here because a mutation test on this very
# guard found nothing stopping the reversion. LogConfigChange used `db.Exec` on the
# raw application pool until 2026-09-05. `config_change_log` has RLS ENABLE +
# FORCE RLS and its policy calls current_setting('app.assigned_books') with no
# missing_ok, and this codebase sets no database- or role-level default for that
# GUC — so on an unprimed pool connection the predicate raises, and on a recycled
# RESET one it tests against '' and the INSERT violates the policy. Both paths
# error, the error degrades to slog.Warn, and the config-history endpoint returns
# an empty list forever. Reverting to `db.Exec` restores that, silently.
#
# NOT a general guard for the class. There are ~38 raw-pool statements touching
# RLS tables in this service; most are on sysPool and correct by design, and the
# rest fail loudly with a 500. The dangerous subset is the ones that swallow the
# error, which is what this list covers.
MUST_USE_REQUEST_CONN = {
    "services/api/internal/humanoverride/humanoverride.go::LogConfigChange": (
        "middleware.DB(ctx, db).Exec",
        "Only writer of config_change_log; swallows its error into slog.Warn, so "
        "on the raw pool the audit table stays empty with no symptom.",
    ),
    "services/api/internal/middleware/auditlog.go::RecordAccess": (
        "DB(ctx, db).Exec",
        "Only writer of access_log; same swallow. Already correct — pinned so it "
        "stays that way.",
    ),
}

# The INET cast idiom. "" from auth.SourceIPFrom must become SQL NULL, not the
# empty string: a row claiming the request came from nowhere is worse than one
# admitting it does not know. This mirrors the proven NULLIF($n,'')::uuid in the
# same statements rather than introducing a pgx inet codec question.
NULLIF_INET = re.compile(r"NULLIF\(\s*\$(\d+)\s*,\s*''\s*\)::inet")


def norm(s):
    """Lowercase alphanumerics only, so `client_book_id` and `clientBookID` meet."""
    return re.sub(r"[^a-z0-9]", "", s.lower())


def column_matches_arg(col, arg):
    """Heuristic: does this Go argument plausibly carry this column's value?

    Compares the first four characters of the normalised column name against the
    normalised argument text. Deliberately weak: many arguments abbreviate
    (`client_book_id` -> `bookID`, `changed_by` -> `userID`) and a strict match
    would fail on correct code. Its only job is to make a genuine SWAP visible —
    see the cross-match check in main(), which fails only when a column matches
    some OTHER position better than its own. An unmatched column is a note.
    """
    c, a = norm(col), norm(arg)
    return len(c) >= 4 and c[:4] in a


def read(rel):
    path = os.path.join(ROOT, rel)
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def split_top(text):
    """Split on commas that are not inside (), '' or ``."""
    parts, depth, buf = [], 0, []
    i, n = 0, len(text)
    while i < n:
        ch = text[i]
        if ch == "'":
            j = text.find("'", i + 1)
            j = n if j < 0 else j
            buf.append(text[i : j + 1])
            i = j + 1
            continue
        if ch == "`":
            j = text.find("`", i + 1)
            j = n if j < 0 else j
            buf.append(text[i : j + 1])
            i = j + 1
            continue
        if ch in "([{":
            depth += 1
        elif ch in ")]}":
            depth -= 1
        if ch == "," and depth == 0:
            parts.append("".join(buf).strip())
            buf = []
            i += 1
            continue
        buf.append(ch)
        i += 1
    tail = "".join(buf).strip()
    if tail:
        parts.append(tail)
    return parts


def call_args(src, open_paren_idx):
    """Return the raw argument text of a call whose '(' is at open_paren_idx."""
    depth, i, n = 0, open_paren_idx, len(src)
    start = open_paren_idx + 1
    while i < n:
        ch = src[i]
        if ch == "`":
            j = src.find("`", i + 1)
            i = n if j < 0 else j + 1
            continue
        if ch == '"':
            j = i + 1
            while j < n and src[j] != '"':
                j += 2 if src[j] == "\\" else 1
            i = j + 1
            continue
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return src[start:i]
        i += 1
    return None


def find_insert(src, table):
    """Locate `INSERT INTO <table> (...) VALUES (...)` and its enclosing call.

    Returns (cols, placeholders, args, sql) or None. `cols` and `args` are lists;
    `placeholders` is the set of distinct $n indexes seen in the VALUES clause.
    """
    m = re.search(r"INSERT\s+INTO\s+" + re.escape(table) + r"\s*\(", src)
    if not m:
        return None
    col_text = call_args(src, m.end() - 1)
    if col_text is None:
        return None
    cols = [c.strip() for c in split_top(col_text) if c.strip()]

    vm = re.search(r"VALUES\s*\(", src[m.end() :])
    if not vm:
        return None
    v_open = m.end() + vm.end() - 1
    values_text = call_args(src, v_open)
    if values_text is None:
        return None
    placeholders = {int(d) for d in re.findall(r"\$(\d+)", values_text)}

    # Walk backwards to the .Exec( / .Query( that owns this literal.
    head = src[: m.start()]
    cm = None
    for cm in re.finditer(r"\.(?:Exec|Query|QueryRow)\(", head):
        pass
    if cm is None:
        return None
    arg_text = call_args(src, cm.end() - 1)
    if arg_text is None:
        return None
    args = split_top(arg_text)
    # arg 0 is the context, arg 1 is the SQL literal; the rest are binds.
    binds = args[2:] if len(args) > 2 else []
    return cols, placeholders, binds, values_text


def enclosing_literal(src, idx):
    """The backtick-delimited SQL literal containing position idx, or None.

    Needed because a non-greedy `SELECT (.*?) FROM <table>` over the whole file
    starts at the FIRST SELECT anywhere and runs to the first matching FROM,
    swallowing every unrelated statement in between. The first version of this
    guard did exactly that and reported 25 selected expressions for a six-column
    query. Bounding the search to one literal is the fix.
    """
    open_at = src.rfind("`", 0, idx)
    if open_at < 0:
        return None
    close_at = src.find("`", idx)
    if close_at < 0:
        return None
    return src[open_at + 1 : close_at], open_at + 1, close_at


def find_select(src, table):
    """Locate `SELECT ... FROM <table>` and the rows.Scan that consumes it."""
    fm = re.search(r"FROM\s+" + re.escape(table) + r"\b", src)
    if not fm:
        return None
    lit = enclosing_literal(src, fm.start())
    if lit is None:
        return None
    body, _, lit_end = lit
    inner = re.search(r"SELECT\s+(.*?)\s+FROM\s+" + re.escape(table) + r"\b", body, re.S)
    if not inner:
        return None
    select_list = [c.strip() for c in split_top(inner.group(1)) if c.strip()]
    sm = re.search(r"rows\.Scan\(", src[lit_end:])
    if not sm:
        return None
    scan_text = call_args(src, lit_end + sm.end() - 1)
    if scan_text is None:
        return None
    targets = [t for t in split_top(scan_text) if t]
    return select_list, targets


def root_chain(src):
    """The r.Use(...) arguments of the ROOT router, in mount order."""
    start = src.find("chi.NewRouter()")
    if start < 0:
        return []
    # The root chain ends at the first sub-router; anything after belongs to a
    # group and is mounted on a different router.
    end = len(src)
    for marker in ("r.Route(", "r.Group(", "r.Mount("):
        idx = src.find(marker, start)
        if idx >= 0:
            end = min(end, idx)
    chain = []
    for um in re.finditer(r"\br\.Use\(", src[start:end]):
        arg = call_args(src[start:end], start + um.end() - 1 - start)
        if arg is not None:
            chain.append(arg.strip())
    return chain


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    failures, notes, checked = [], [], 0

    # ---- 1. writes: arity, and source_ip present by name ----
    for key, why in WRITES.items():
        rel, table = key.split("::")
        try:
            src = read(rel)
        except OSError as exc:
            failures.append(f"{key}: cannot read source ({exc})")
            continue
        found = find_insert(src, table)
        if found is None:
            failures.append(
                f"{key}: no parseable `INSERT INTO {table} (...) VALUES (...)`. "
                f"Either the statement moved or this guard's parser has drifted; "
                f"both need a human. Reason this site matters: {why}"
            )
            continue
        cols, placeholders, binds, values_text = found
        checked += 1

        if "source_ip" not in cols:
            failures.append(
                f"{key}: `source_ip` is no longer in the INSERT column list "
                f"({', '.join(cols)}). {why}"
            )
        if not NULLIF_INET.search(values_text):
            failures.append(
                f"{key}: source_ip must be bound as NULLIF($n, '')::inet so that "
                f'an unknown address is SQL NULL and not the string "". VALUES '
                f"clause is: {' '.join(values_text.split())}"
            )
        if len(cols) != len(placeholders):
            failures.append(
                f"{key}: {len(cols)} column(s) but {len(placeholders)} distinct "
                f"placeholder(s) {sorted(placeholders)} — pgx binds by position."
            )
        if placeholders and sorted(placeholders) != list(range(1, len(placeholders) + 1)):
            failures.append(
                f"{key}: placeholders are not a contiguous $1..$n run: "
                f"{sorted(placeholders)}"
            )
        if len(binds) != len(placeholders):
            failures.append(
                f"{key}: {len(placeholders)} placeholder(s) but {len(binds)} Go "
                f"argument(s) after the SQL literal ({', '.join(binds) or 'none'})."
            )

        # Arity agreeing is necessary and NOT sufficient: the same count in the
        # wrong order raises no error at all and writes a confidently wrong audit
        # row, which for this product is worse than writing none. This fails only
        # on a positive cross-match — column i not matching arg i but matching
        # arg j — so that legitimate abbreviations stay notes, not failures.
        if len(binds) == len(cols):
            unmatched = []
            for i, col in enumerate(cols):
                if column_matches_arg(col, binds[i]):
                    continue
                others = [
                    j for j in range(len(binds))
                    if j != i and column_matches_arg(col, binds[j])
                ]
                if others:
                    failures.append(
                        f"{key}: column {i + 1} `{col}` does not match its own "
                        f"argument `{binds[i]}` but does match argument "
                        f"{others[0] + 1} `{binds[others[0]]}` — the bind list "
                        f"looks transposed. pgx binds by position and Postgres "
                        f"will accept this silently."
                    )
                else:
                    unmatched.append(col)
            if unmatched and args.verbose:
                notes.append(
                    f"{key}: {len(unmatched)} column(s) could not be matched to an "
                    f"argument by name ({', '.join(unmatched)}) — abbreviated "
                    f"identifiers, so transposition among these is NOT covered."
                )
        if args.verbose and not failures:
            print(
                f"  ok  {key}: {len(cols)} cols == {len(placeholders)} placeholders "
                f"== {len(binds)} args; source_ip bound with the ::inet NULLIF cast"
            )

    # ---- 2. reads: select list vs Scan targets ----
    for key, why in READS.items():
        rel, table = key.split("::")
        src = read(rel)
        found = find_select(src, table)
        if found is None:
            failures.append(f"{key}: no parseable SELECT ... FROM {table} + rows.Scan. {why}")
            continue
        select_list, targets = found
        checked += 1
        if not any("source_ip" in c for c in select_list):
            failures.append(
                f"{key}: `source_ip` is no longer selected, so the endpoint that "
                f"answers 'from where' stopped answering it. {why}"
            )
        if len(select_list) != len(targets):
            failures.append(
                f"{key}: {len(select_list)} selected expression(s) but "
                f"{len(targets)} Scan target(s). rows.Scan binds by position and "
                f"this loop `continue`s on error, so this returns an empty list "
                f"with a 200 rather than failing. {why}"
            )
        elif args.verbose:
            print(
                f"  ok  {key}: {len(select_list)} selected == {len(targets)} "
                f"Scan target(s); source_ip among them"
            )

    # ---- 3. audit writes must use the RLS-wired request connection ----
    for key, (needle, why) in MUST_USE_REQUEST_CONN.items():
        rel, func = key.split("::")
        src = read(rel)
        fm = re.search(r"func (?:\([^)]*\) )?" + re.escape(func) + r"\(", src)
        if not fm:
            failures.append(
                f"{key}: function not found. It was pinned here because reverting "
                f"it is silent. {why}"
            )
            continue
        # Body runs to the next top-level func, which is close enough: these are
        # both short helpers and a false window would show up as a failure, not a
        # pass.
        nxt = re.search(r"\nfunc ", src[fm.end():])
        body = src[fm.end(): fm.end() + (nxt.start() if nxt else len(src))]
        checked += 1
        if needle not in body:
            failures.append(
                f"{key}: expected `{needle}` and did not find it — this write is "
                f"back on a connection with no tenant GUCs set. {why}"
            )
        elif re.search(r"(?<!middleware\.)\bdb\.Exec\(", body):
            failures.append(
                f"{key}: still contains a raw `db.Exec(` alongside the request-"
                f"connection call. One of them is writing unprimed. {why}"
            )
        elif args.verbose:
            print(f"  ok  {key}: writes through `{needle}`")

    # ---- 4. middleware order in the root chain ----
    src = read(MAIN_GO)
    chain = root_chain(src)
    real_at = next((i for i, u in enumerate(chain) if "RealIP" in u), None)
    src_at = next((i for i, u in enumerate(chain) if "middleware.SourceIP" in u), None)
    if not chain:
        failures.append(
            f"{MAIN_GO}: parsed ZERO r.Use() entries in the root chain. The guard "
            f"has gone inert — this is a failure, not a pass."
        )
    elif src_at is None:
        failures.append(
            f"{MAIN_GO}: middleware.SourceIP is not mounted on the root chain "
            f"({len(chain)} entries parsed), so every audit row records NULL."
        )
    elif real_at is None:
        failures.append(f"{MAIN_GO}: middleware.RealIP is not mounted on the root chain.")
    elif src_at < real_at:
        failures.append(
            f"{MAIN_GO}: middleware.SourceIP is mounted at position {src_at}, "
            f"ABOVE middleware.RealIP at {real_at}. It copies the rewritten "
            f"RemoteAddr, so above RealIP it records the proxy's address instead "
            f"of the client's — silently, on every request behind a proxy."
        )
    else:
        checked += 1
        if src_at != real_at + 1:
            notes.append(
                f"{MAIN_GO}: SourceIP is after RealIP (correct) but not directly "
                f"after it ({real_at} -> {src_at}). Not a failure; worth a look."
            )
        if args.verbose:
            print(f"  ok  {MAIN_GO}: RealIP at {real_at}, SourceIP at {src_at} (after)")

    if checked == 0:
        failures.append(
            "INERT: no site was parsed at all. A guard that checks nothing must "
            "fail, or it reports success forever after the code moves."
        )

    print(f"\n{checked} site(s) checked across {len(WRITES)} write(s), "
          f"{len(READS)} read(s), {len(MUST_USE_REQUEST_CONN)} connection "
          f"pin(s) and 1 middleware chain")
    for n in notes:
        print(f"NOTE — {n}")
    if failures:
        print("\nFAIL:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("OK: audit source_ip present, every bind list agrees, chain order correct")
    return 0


if __name__ == "__main__":
    sys.exit(main())
