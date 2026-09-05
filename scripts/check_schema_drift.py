#!/usr/bin/env python3
"""Fail the build when application SQL references schema that does not exist.

Why this exists
---------------
infra/init.sql is the ONLY DDL any environment applies. There is no migration
runner. Before this guard, services/api/db/migrations/ held 8 .up.sql files that
nothing ever executed, so 6 tables, 1 view and 10 columns were referenced by
live handlers but absent from the applied schema. Nothing in CI could see it:

  * `sqlc compile` only type-checks db/queries/ (8 of ~169 DB call sites); every
    other query is a raw pgx string literal, invisible to it.
  * `go build` / `go vet` / lint never parse SQL string literals.
  * the one real-database test suite died in setup on a missing table, so the
    schema failure masked the RLS failure behind it.

So: parse init.sql, extract every table/view/column reference out of the Go and
Python sources, and diff. Deliberately conservative — it only reports a column
when it can attribute it to exactly one table without guessing, because a false
positive that people learn to ignore is worse than a narrower check.

Usage:  python3 scripts/check_schema_drift.py [--verbose]
Exit 0 = no drift, 1 = drift found, 2 = could not parse the schema at all.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from collections import defaultdict

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCHEMA_FILE = os.path.join(REPO, "infra", "init.sql")
SOURCE_DIRS = [
    os.path.join(REPO, "services", "api"),
    os.path.join(REPO, "services", "agent-runtime"),
]
SOURCE_EXTS = (".go", ".py")

# Databases created by init.sql for the observability sidecars. Their schemas are
# managed by those tools, not by us, so nothing here should reference them.
FOREIGN_DATABASES = {"langfuse", "glitchtip"}

# Set-returning functions and pseudo-relations that appear where a table name
# would. Not schema, so not drift.
NOT_TABLES = {
    "unnest", "generate_series", "jsonb_array_elements", "json_array_elements",
    "jsonb_to_recordset", "string_to_array", "regexp_split_to_table", "values",
    "lateral", "only", "dual",
}


# System catalogs are matched by RULE, not by enumeration. The first version of
# this file listed pg_class/pg_roles/pg_policy individually and then reported
# `pg_policies` — referenced by cmd/server/rolecheck.go:138 — as schema drift,
# because a list of names only covers the catalogs someone happened to think of.
# Every catalog relation is either pg_-prefixed or lives in information_schema,
# so the prefix test cannot fall behind the code the way the list did.
def is_system_catalog(name: str) -> bool:
    n = name.lower().lstrip('"').split(".")[0]
    return n.startswith("pg_") or n == "information_schema"


# Columns every table effectively has for our purposes.
PSEUDO_COLUMNS = {"ctid", "xmin", "tableoid", "oid"}


# --------------------------------------------------------------------------- #
# Schema side
# --------------------------------------------------------------------------- #

def strip_sql_comments(text: str) -> str:
    """Remove -- line comments. Keeps string literals intact well enough for DDL."""
    out = []
    for line in text.splitlines():
        idx = line.find("--")
        if idx == 0:
            continue
        if idx > 0 and line.count("'", 0, idx) % 2 == 0:
            line = line[:idx]
        out.append(line)
    return "\n".join(out)


COLUMN_CONSTRAINT_WORDS = {
    "primary", "unique", "check", "foreign", "constraint", "exclude", "like",
}


def parse_create_table(body: str) -> list[str]:
    """Return the column names from the parenthesised body of a CREATE TABLE."""
    cols, depth, current = [], 0, []
    for ch in body:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            cols.append("".join(current))
            current = []
        else:
            current.append(ch)
    cols.append("".join(current))

    names = []
    for raw in cols:
        tok = raw.strip().split()
        if not tok:
            continue
        first = tok[0].strip('"').lower()
        if first in COLUMN_CONSTRAINT_WORDS:
            continue
        if re.fullmatch(r"[a-z_][a-z0-9_]*", first):
            names.append(first)
    return names


def parse_view_columns(body: str) -> list[str]:
    """Output column names of a simple SELECT-list view."""
    m = re.search(r"\bSELECT\b(.*?)\bFROM\b", body, re.S | re.I)
    if not m:
        return []
    items, depth, current = [], 0, []
    for ch in m.group(1):
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            items.append("".join(current))
            current = []
        else:
            current.append(ch)
    items.append("".join(current))

    names = []
    for item in items:
        item = item.strip()
        alias = re.search(r"\bAS\s+([a-z_][a-z0-9_]*)\s*$", item, re.I)
        if alias:
            names.append(alias.group(1).lower())
        elif re.fullmatch(r"[a-z_][a-z0-9_.]*", item):
            names.append(item.split(".")[-1].lower())
    return names


def load_schema() -> tuple[dict[str, set[str]], set[str]]:
    """Return ({relation: {columns}}, {view names}) from infra/init.sql."""
    with open(SCHEMA_FILE, encoding="utf-8") as fh:
        text = strip_sql_comments(fh.read())

    relations: dict[str, set[str]] = {}
    views: set[str] = set()

    for m in re.finditer(
        r"^CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s*\((.*?)^\);",
        text, re.S | re.M | re.I,
    ):
        relations[m.group(1).lower()] = set(parse_create_table(m.group(2)))

    for m in re.finditer(
        r"^CREATE\s+(?:OR\s+REPLACE\s+)?VIEW\s+([a-z_][a-z0-9_]*)\b(.*?);\s*$",
        text, re.S | re.M | re.I,
    ):
        name = m.group(1).lower()
        views.add(name)
        relations[name] = set(parse_view_columns(m.group(2)))

    return relations, views


# --------------------------------------------------------------------------- #
# Source side
# --------------------------------------------------------------------------- #

SQL_START = re.compile(
    r"^\s*(?:--.*\n\s*)*(?:WITH|SELECT|INSERT|UPDATE|DELETE|TRUNCATE)\b", re.I)

# A literal must ALSO match one of these to be treated as SQL. Without this,
# ordinary Go error strings — "update failed: %w", "select failed" — parse as
# `UPDATE failed` and get reported as a missing table named "failed".
SQL_SHAPE = [
    re.compile(r"^\s*SELECT\b.*?\bFROM\b", re.I | re.S),
    re.compile(r"^\s*INSERT\s+INTO\s+[a-z_\"]", re.I),
    re.compile(r"^\s*UPDATE\s+[a-z_][a-z0-9_]*\s+SET\b", re.I),
    re.compile(r"^\s*DELETE\s+FROM\s+[a-z_\"]", re.I),
    re.compile(r"^\s*TRUNCATE\s+(?:TABLE\s+)?[a-z_\"]", re.I),
    re.compile(r"^\s*WITH\s+[a-z_][a-z0-9_]*\s+AS\s*\(", re.I),
]


def looks_like_sql(body: str) -> bool:
    if not SQL_START.match(body):
        return False
    return any(p.search(body) for p in SQL_SHAPE)


# Go raw strings (backticks), Go/Python double-quoted, Python triple-quoted.
LITERAL_PATTERNS = [
    re.compile(r"`([^`]*)`", re.S),
    re.compile(r'"""(.*?)"""', re.S),
    re.compile(r'"((?:[^"\\\n]|\\.)*)"'),
]


def extract_sql_literals(path: str) -> list[tuple[int, str]]:
    """Every string literal in the file that looks like a SQL statement."""
    with open(path, encoding="utf-8", errors="replace") as fh:
        src = fh.read()
    found = []
    for pat in LITERAL_PATTERNS:
        for m in pat.finditer(src):
            body = m.group(1)
            if not looks_like_sql(body):
                continue
            line = src.count("\n", 0, m.start()) + 1
            found.append((line, body))
    return found


TABLE_REF = re.compile(
    r"\b(?:FROM|JOIN|INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(?:\s+TABLE)?)\s+"
    r"((?:[a-z_][a-z0-9_]*\s*,\s*)*[a-z_][a-z0-9_]*)",
    re.I)
CTE_DEF = re.compile(r"\b(?:WITH|,)\s+([a-z_][a-z0-9_]*)\s+AS\s*\(", re.I)
ALIAS_DEF = re.compile(
    r"\b(?:FROM|JOIN|UPDATE)\s+([a-z_][a-z0-9_]*)\s+(?:AS\s+)?([a-z_][a-z0-9_]*)\b",
    re.I)

RESERVED_AFTER_TABLE = {
    "set", "where", "select", "values", "on", "using", "join", "left", "right",
    "inner", "outer", "full", "cross", "group", "order", "limit", "returning",
    "as", "and", "or", "having", "union", "except", "intersect", "for", "do",
    "conflict", "cascade", "restart", "identity",
}

# Words that can appear as bare identifiers in a statement without being column
# names. Used only by the single-table attribution pass below, so it needs to be
# generous: a missing entry here is a false "missing column", which is the one
# failure mode that would make people stop trusting this guard.
SQL_NOISE = RESERVED_AFTER_TABLE | {
    "insert", "into", "update", "delete", "from", "truncate", "with", "distinct",
    "all", "any", "some", "exists", "in", "is", "not", "null", "true", "false",
    "case", "when", "then", "else", "end", "between", "like", "ilike", "similar",
    "escape", "asc", "desc", "nulls", "first", "last", "offset", "fetch", "next",
    "rows", "row", "only", "table", "column", "constraint", "default", "cast",
    "interval", "day", "days", "hour", "hours", "minute", "minutes", "second",
    "seconds", "month", "months", "year", "years", "week", "weeks", "epoch",
    "at", "time", "zone", "timezone", "local", "current_date", "current_time",
    "current_timestamp", "localtime", "localtimestamp", "session_user", "user",
    "nothing", "excluded", "returning", "filter", "over", "partition", "by",
    "window", "recursive", "lateral", "natural", "unique", "primary", "key",
    "foreign", "references", "check", "collate", "array", "unnest", "of",
    "text", "integer", "int", "int4", "int8", "bigint", "smallint", "boolean",
    "bool", "uuid", "date", "timestamp", "timestamptz", "numeric", "decimal",
    "real", "double", "precision", "jsonb", "json", "bytea", "char", "varchar",
    "bigserial", "serial", "regclass", "record", "void", "trigger",
}



def referenced_tables(sql: str) -> set[str]:
    ctes = {m.group(1).lower() for m in CTE_DEF.finditer(sql)}
    out = set()
    for m in TABLE_REF.finditer(sql):
        for name in m.group(1).split(","):
            name = name.strip().lower()
            if not name or name in ctes or name in NOT_TABLES:
                continue
            if is_system_catalog(name):
                continue
            if name in RESERVED_AFTER_TABLE:
                continue
            out.add(name)
    return out


def referenced_columns(sql: str, known: dict[str, set[str]]) -> set[tuple[str, str]]:
    """(table, column) pairs we can attribute with certainty. Conservative."""
    pairs: set[tuple[str, str]] = set()

    # INSERT INTO t (a, b, c)
    for m in re.finditer(
        r"\bINSERT\s+INTO\s+([a-z_][a-z0-9_]*)\s*\(([^)]*)\)", sql, re.I):
        table = m.group(1).lower()
        for col in m.group(2).split(","):
            col = col.strip().strip('"').lower()
            if re.fullmatch(r"[a-z_][a-z0-9_]*", col):
                pairs.add((table, col))

    # UPDATE t SET a = ..., b = ...   (stop at WHERE/RETURNING/FROM)
    for m in re.finditer(
        r"\bUPDATE\s+([a-z_][a-z0-9_]*)\s+SET\s+(.*?)(?:\bWHERE\b|\bRETURNING\b|\bFROM\b|$)",
        sql, re.I | re.S):
        table = m.group(1).lower()
        for assign in re.finditer(r"([a-z_][a-z0-9_]*)\s*=", m.group(2), re.I):
            pairs.add((table, assign.group(1).lower()))

    # alias.column / table.column, where the alias resolves to one known table
    aliases: dict[str, str] = {}
    for m in ALIAS_DEF.finditer(sql):
        table, alias = m.group(1).lower(), m.group(2).lower()
        if alias in RESERVED_AFTER_TABLE or table not in known:
            continue
        aliases[alias] = table
    for t in referenced_tables(sql):
        aliases.setdefault(t, t)
    for m in re.finditer(r"\b([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)\b", sql):
        owner, col = m.group(1).lower(), m.group(2).lower()
        if owner in aliases and col not in PSEUDO_COLUMNS:
            pairs.add((aliases[owner], col))

    pairs |= single_table_columns(sql, known)
    return pairs


def single_table_columns(sql: str, known: dict[str, set[str]]) -> set[tuple[str, str]]:
    """Bare (unqualified) identifiers in a statement that touches ONE table.

    This is the pass that catches `SELECT invite_token, invite_expires FROM
    client_portal_users` — a shape the INSERT/UPDATE/qualified passes all miss,
    and the shape that broke portal login. Restricted to single-table, no-CTE,
    no-subquery statements so that attribution is never a guess.
    """
    tables = referenced_tables(sql)
    if len(tables) != 1:
        return set()
    table = next(iter(tables))
    if table not in known:
        return set()
    if CTE_DEF.search(sql) or re.search(r"\(\s*SELECT\b", sql, re.I):
        return set()

    # Drop string literals, casts, dollar params, and function calls, then treat
    # what is left as candidate column names.
    body = re.sub(r"'(?:[^']|'')*'", " ", sql)
    body = re.sub(r"::\s*[a-z_][a-z0-9_]*(\s*\[\s*\])?", " ", body, flags=re.I)
    body = re.sub(r"\$\d+", " ", body)
    body = re.sub(r"\b[a-z_][a-z0-9_]*\s*\(", " ( ", body, flags=re.I)
    body = re.sub(r"\b[a-z_][a-z0-9_]*\s*\.\s*[a-z_][a-z0-9_]*", " ", body, flags=re.I)

    # `FROM client_books b` / `UPDATE t AS x` — the trailing token is a table
    # alias, not a column.
    table_aliases = {
        m.group(1).lower()
        for m in re.finditer(
            r"\b(?:FROM|JOIN|UPDATE|INTO)\s+" + re.escape(table) +
            r"\s+(?:AS\s+)?([a-z_][a-z0-9_]*)", body, re.I)
        if m.group(1).lower() not in RESERVED_AFTER_TABLE
    }

    aliased = {m.group(1).lower()
               for m in re.finditer(r"\bAS\s+([a-z_][a-z0-9_]*)", body, re.I)}
    aliased |= table_aliases

    out = set()
    for m in re.finditer(r"\b([a-z_][a-z0-9_]*)\b", body):
        name = m.group(1).lower()
        if name in SQL_NOISE or name in PSEUDO_COLUMNS or name in aliased:
            continue
        if name == table or name in known:
            continue
        out.add((table, name))
    return out



# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #

def iter_sources():
    for root_dir in SOURCE_DIRS:
        for root, dirs, files in os.walk(root_dir):
            dirs[:] = [d for d in dirs if d not in
                       {".git", "node_modules", "target", "vendor", "__pycache__",
                        ".venv", ".pytest_cache"}]
            for name in sorted(files):
                if name.endswith(SOURCE_EXTS):
                    yield os.path.join(root, name)


def main() -> int:
    global SCHEMA_FILE

    ap = argparse.ArgumentParser()
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--schema", default=SCHEMA_FILE,
                    help="alternate schema file; used to self-test that this "
                         "guard actually fails on a known-bad schema")
    args = ap.parse_args()

    SCHEMA_FILE = args.schema

    if not os.path.exists(SCHEMA_FILE):
        print(f"FATAL: schema file not found: {SCHEMA_FILE}", file=sys.stderr)
        return 2

    relations, views = load_schema()
    if len(relations) < 10:
        print(f"FATAL: parsed only {len(relations)} relations from init.sql — the "
              f"parser is broken, refusing to report a green result", file=sys.stderr)
        return 2

    missing_tables: dict[str, list[str]] = defaultdict(list)
    missing_columns: dict[tuple[str, str], list[str]] = defaultdict(list)
    used_tables: set[str] = set()
    n_files = n_stmts = 0

    for path in iter_sources():
        rel = os.path.relpath(path, REPO)
        literals = extract_sql_literals(path)
        if literals:
            n_files += 1
        for line, sql in literals:
            n_stmts += 1
            where = f"{rel}:{line}"
            tables = referenced_tables(sql)
            used_tables |= tables
            for t in sorted(tables):
                if t in FOREIGN_DATABASES:
                    continue
                if t not in relations:
                    missing_tables[t].append(where)
            for table, col in sorted(referenced_columns(sql, relations)):
                if table in relations and col not in relations[table]:
                    missing_columns[(table, col)].append(where)

    return report(relations, views, used_tables, missing_tables,
                  missing_columns, n_files, n_stmts, args.verbose)


def report(relations, views, used_tables, missing_tables, missing_columns,
           n_files, n_stmts, verbose) -> int:
    print(f"schema:  {len(relations) - len(views)} tables + {len(views)} view(s) "
          f"parsed from infra/init.sql")
    print(f"sources: {n_stmts} SQL literal(s) across {n_files} file(s)")

    failed = False

    if missing_tables:
        failed = True
        print(f"\nDRIFT — {len(missing_tables)} relation(s) referenced by code but "
              f"absent from infra/init.sql:")
        for table in sorted(missing_tables):
            sites = missing_tables[table]
            print(f"  {table}")
            for site in sites[:8]:
                print(f"      {site}")
            if len(sites) > 8:
                print(f"      ... and {len(sites) - 8} more")

    if missing_columns:
        failed = True
        print(f"\nDRIFT — {len(missing_columns)} column(s) referenced by code but "
              f"absent from that relation in infra/init.sql:")
        for (table, col) in sorted(missing_columns):
            sites = missing_columns[(table, col)]
            print(f"  {table}.{col}")
            for site in sites[:8]:
                print(f"      {site}")
            if len(sites) > 8:
                print(f"      ... and {len(sites) - 8} more")

    unused = sorted(set(relations) - used_tables)
    if unused:
        # Informational only. Reference data and future-phase tables legitimately
        # have no reader yet; failing on this would just train people to ignore it.
        print(f"\nNOTE — {len(unused)} relation(s) in the schema with no SQL "
              f"reference in services/api or services/agent-runtime:")
        print("  " + ", ".join(unused))

    if verbose:
        print("\nrelations parsed:")
        for name in sorted(relations):
            kind = "view " if name in views else "table"
            print(f"  {kind} {name} ({len(relations[name])} cols)")

    print("\nFAIL: schema drift detected" if failed else "\nOK: no schema drift")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())






