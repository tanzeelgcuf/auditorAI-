#!/usr/bin/env python3
"""Guard the psql/SQL split between infra/init.sql and infra/00-bootstrap-roles.sql.

THE BUG THIS EXISTS FOR (2026-09-06). `services/api/sqlc.yaml` sets
`schema: "../../infra/init.sql"`, so sqlc parses that file with the real
PostgreSQL parser. init.sql contained four psql CLIENT meta-commands and a psql
variable interpolation:

    \\set app_pw ''
    \\getenv app_pw APP_DB_PASSWORD
    SELECT set_config('auditor.bootstrap_app_pw', :'app_pw', false);

None of that is SQL. `sqlc compile` failed, and because that step precedes
`go test` in the Go job, EVERY Go test — including the entire DATABASE_URL_TEST
integration suite — never ran. The CI log reported the position as `859:48`,
which reads like a stray colon; it is not. Line 859 was the first `\\set` (14
characters long, so it has no column 48) and 48 is the column of `:'app_pw'`'s
opening quote on line 863, the first statement-terminating `;` after it. Fixing
"the colon" would have broken database init and left sqlc just as broken.

The fix moved the credential bootstrap to infra/00-bootstrap-roles.sql, which
psql runs first and sqlc never reads.

WHY THE POSITIVE HALF IS HERE. `\\getenv` is a security control, not a style
choice: the alternative form, `\\set app_pw \\`printf '%s' "$APP_DB_PASSWORD"\\``,
makes psql run a SHELL, and inside double quotes a shell still expands $(...)
and backticks — so a password containing either would be EXECUTED during
database init. A guard that only rejected meta-commands in init.sql would be
perfectly happy if someone "simplified" the bootstrap back to the shell form.
So this checks both directions: the constructs must be absent from the schema
file AND still present, in the safe form, in the bootstrap file.

Exit 0 = split intact. Exit 1 = violation. Exit 2 = the guard could not find
what it checks and is therefore inert, which must never be read as a pass.
"""

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SQLC_YAML = os.path.join(ROOT, "services", "api", "sqlc.yaml")
BOOTSTRAP = os.path.join(ROOT, "infra", "00-bootstrap-roles.sql")

# Every place that applies the schema must apply the bootstrap first.
APPLIERS = [
    os.path.join(ROOT, "infra", "docker-compose.yml"),
    os.path.join(ROOT, "infra", "docker-compose.dev.yml"),
    os.path.join(ROOT, ".github", "workflows", "ci.yml"),
]

problems = []
inert = []


def read(path):
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def code_lines(text):
    """(1-based lineno, line) for lines that are not pure `--` SQL comments."""
    out = []
    for i, line in enumerate(text.split("\n"), start=1):
        if line.lstrip().startswith("--"):
            continue
        out.append((i, line))
    return out


# ---------------------------------------------------------------- schema target
# Resolved from sqlc.yaml rather than hardcoded, so that repointing sqlc at a
# different file moves this guard with it instead of leaving it checking a file
# nobody parses any more.
def schema_target():
    if not os.path.exists(SQLC_YAML):
        inert.append(f"sqlc.yaml not found at {SQLC_YAML}")
        return None
    m = re.search(r'^\s*schema:\s*["\']?([^"\'\n]+)["\']?\s*$',
                  read(SQLC_YAML), re.MULTILINE)
    if not m:
        inert.append("no `schema:` key in sqlc.yaml — cannot tell what sqlc parses")
        return None
    target = os.path.normpath(
        os.path.join(os.path.dirname(SQLC_YAML), m.group(1).strip()))
    if not os.path.exists(target):
        inert.append(f"sqlc.yaml schema target does not exist: {target}")
        return None
    return target


SCHEMA = schema_target()

# ------------------------------------------------- NEGATIVE half: schema is SQL
# psql accepts a meta-command at the start of a line OR immediately after a `;`
# on the same line, so a line-start check alone is not enough. Found while
# mutant-testing this guard: init.sql did not end in a newline, so appending
# "\\set x 1" produced `$selfcheck$;\\set x 1` — a line that psql would happily
# execute and the first version of this check scored clean. The mutant was a bad
# mutant AND the guard had a real hole; both are fixed.
PSQL_META = re.compile(
    r"(?:^|;)\s*\\(set|unset|getenv|setenv|gset|gexec|if|elif|else|endif|echo"
    r"|warn|i|ir|include|include_relative|copy|connect|c|password|timing|pset"
    r"|o|out|g|watch|prompt|cd|crosstabview|d[a-zA-Z+]*)\b")

if SCHEMA:
    text = read(SCHEMA)
    rel = os.path.relpath(SCHEMA, ROOT)
    for lineno, line in code_lines(text):
        m = PSQL_META.search(line)
        if m:
            problems.append(
                f"{rel}:{lineno}:{m.start(1)}: psql meta-command "
                f"\\{m.group(1)} in a file sqlc parses. sqlc cannot read it and "
                f"`sqlc compile` will fail, taking the whole Go job — every test "
                f"included — with it. Put it in infra/00-bootstrap-roles.sql.")
        if ":'" in line:
            col = line.index(":'") + 2
            problems.append(
                f"{rel}:{lineno}:{col}: psql variable interpolation :'...' in a "
                f"file sqlc parses. This is the exact construct that reported as "
                f"`859:48` and read like a stray colon. It is not a typo, and "
                f"deleting the colon breaks database init without fixing sqlc.")

# ------------------------- POSITIVE half: the fix itself, pinned by name
# A guard that only rejects NEW meta-commands in init.sql lets the fixed instance
# rot back — the bootstrap could be deleted, renamed so it sorts last, or
# "simplified" to the shell form, and every check above would still pass.
if not os.path.exists(BOOTSTRAP):
    problems.append(
        "infra/00-bootstrap-roles.sql is MISSING. init.sql GRANTs to auditor_app "
        "and auditor_sys and RAISEs at the end if either is absent, so without "
        "this file database init fails everywhere. If the bootstrap moved, update "
        "this guard in the same commit.")
else:
    boot = read(BOOTSTRAP)
    brel = os.path.relpath(BOOTSTRAP, ROOT)

    # The injection control. \getenv reads the variable with no shell; the
    # backquote form runs one.
    for var, env in (("app_pw", "APP_DB_PASSWORD"), ("sys_pw", "SYS_DB_PASSWORD")):
        if f"\\getenv {var} {env}" not in boot:
            problems.append(
                f"{brel}: `\\getenv {var} {env}` is gone. Passwords must be read "
                f"with \\getenv, which involves no shell. Reading them any other "
                f"way is a change to an injection control, not a cleanup.")
        if f"\\set {var} ''" not in boot:
            problems.append(
                f"{brel}: `\\set {var} ''` is gone. \\getenv leaves a psql "
                f"variable UNCHANGED when the env var is absent, so without this "
                f"default :'{var}' is emitted literally and the operator gets a "
                f"syntax error instead of the DO block's actionable message.")

    if re.search(r"\\set\s+\w+\s+`", boot):
        problems.append(
            f"{brel}: backquote form found (`\\set x \\`cmd\\``). psql runs a "
            f"SHELL for that, and inside double quotes a shell still expands "
            f"$(...) and backticks — a password containing either would be "
            f"EXECUTED during database init. Use \\getenv.")

    for role, attr in (("auditor_app", "NOBYPASSRLS"), ("auditor_sys", "BYPASSRLS")):
        if f"CREATE ROLE {role}" not in boot:
            problems.append(f"{brel}: no `CREATE ROLE {role}`.")
        elif attr not in boot:
            problems.append(
                f"{brel}: {role} is created but {attr} is absent from this file. "
                f"auditor_app without NOBYPASSRLS makes every RLS policy in "
                f"init.sql inert; auditor_sys without BYPASSRLS 500s pre-auth "
                f"login and every cross-firm worker.")

    # Schema stays in exactly one file.
    for kw in ("CREATE TABLE", "CREATE VIEW", "CREATE INDEX", "CREATE POLICY",
               "CREATE MATERIALIZED VIEW"):
        if kw in boot:
            problems.append(
                f"{brel}: `{kw}` here. sqlc and scripts/check_schema_drift.py "
                f"both read init.sql as the ONLY source of relations and neither "
                f"parses this file, so a relation defined here is invisible to "
                f"both. Move it to init.sql.")

    # Lexical order is what makes the postgres entrypoint glob run this first.
    if SCHEMA and os.path.basename(BOOTSTRAP) >= os.path.basename(SCHEMA):
        problems.append(
            f"{brel} does not sort before {os.path.basename(SCHEMA)}. The postgres "
            f"entrypoint globs /docker-entrypoint-initdb.d/* in lexical order, so "
            f"this file must sort first or init.sql GRANTs to roles that do not "
            f"exist yet.")

# ------------------- POSITIVE half, part 2: every applier applies both files
for path in APPLIERS:
    if not os.path.exists(path):
        inert.append(f"applier not found: {os.path.relpath(path, ROOT)}")
        continue
    text = read(path)
    arel = os.path.relpath(path, ROOT)
    applies_schema = "init.sql" in text
    applies_boot = "00-bootstrap-roles.sql" in text
    if applies_schema and not applies_boot:
        problems.append(
            f"{arel} applies init.sql but never 00-bootstrap-roles.sql. init.sql "
            f"GRANTs to auditor_app/auditor_sys and its final DO block RAISEs if "
            f"they are missing, so this path fails on a fresh database.")
    elif applies_schema and applies_boot:
        # In ci.yml the two psql invocations are sequential, so textual order IS
        # execution order. In compose the entrypoint sorts by filename, but a
        # reader who sees init.sql listed first will assume it runs first, and the
        # next person to add a third file will copy whatever order they see.
        boot_at = text.index("00-bootstrap-roles.sql")
        # The first mention of init.sql that is NOT part of "00-bootstrap-roles"
        # and not a bare prose reference inside a comment line.
        schema_at = None
        for i, line in enumerate(text.split("\n")):
            if "init.sql" in line and not line.lstrip().startswith("#"):
                schema_at = text.index(line)
                break
        if schema_at is not None and schema_at < boot_at:
            problems.append(
                f"{arel}: init.sql is applied before 00-bootstrap-roles.sql. "
                f"Reverse them — the roles must exist before the GRANTs run.")

# ------------------------------------------------------- INERTNESS gate, exit 2
# A guard that silently checked nothing has historically been read as a pass in
# this repo. If it could not resolve what it inspects, that is a distinct outcome
# from "clean".
if inert:
    print("INERT: this guard could not find what it checks, so its silence means "
          "nothing:", file=sys.stderr)
    for msg in inert:
        print(f"  - {msg}", file=sys.stderr)
    sys.exit(2)

if problems:
    print(f"BOOTSTRAP SPLIT VIOLATION ({len(problems)}):", file=sys.stderr)
    for msg in problems:
        print(f"  - {msg}", file=sys.stderr)
    print("\nDo not resolve this by pointing sqlc somewhere else or by deleting "
          "the bootstrap. init.sql must be parseable SQL; the credential read "
          "must stay in psql with \\getenv.", file=sys.stderr)
    sys.exit(1)

schema_rel = os.path.relpath(SCHEMA, ROOT) if SCHEMA else "?"
boot_lines = len(read(BOOTSTRAP).split("\n")) - 1
print(f"OK: {schema_rel} is parseable SQL (0 psql meta-commands, 0 :'var' "
      f"interpolations); infra/00-bootstrap-roles.sql ({boot_lines} lines) still "
      f"reads both passwords with \\getenv and creates both roles; "
      f"{len(APPLIERS)} appliers all run it first.")
