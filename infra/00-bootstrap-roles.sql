-- infra/00-bootstrap-roles.sql — the two database roles, and NOTHING else.
--
-- WHY THIS IS A SEPARATE FILE, and do not fold it back into init.sql.
--
-- `services/api/sqlc.yaml` declares `schema: "../../infra/init.sql"`, so sqlc
-- parses that file with the real PostgreSQL parser (pg_query) to type every
-- query in db/queries/. Four lines of this bootstrap are psql CLIENT
-- meta-commands and one construct is psql variable interpolation:
--
--     \set app_pw ''
--     \getenv app_pw APP_DB_PASSWORD
--     SELECT set_config('auditor.bootstrap_app_pw', :'app_pw', false);
--                                                   ^^^^^^^^^
--
-- None of that is SQL. A PostgreSQL parser rejects it, so `sqlc compile` failed
-- on init.sql, and because that step precedes `go test` in the Go job, the whole
-- Go build — including the DATABASE_URL_TEST suite — never ran.
--
-- Measured 2026-09-06: on the old init.sql, `:'app_pw'`'s opening quote sat at
-- COLUMN 48 of the line, and the statement it belongs to began at LINE 859 (the
-- first `\set`, because nothing terminates a statement until the `;` on 863).
-- That is exactly the `859:48` position the CI log reported. It was read as "a
-- stray colon" — it is not a typo, it is psql interpolation, and deleting the
-- colon would have broken database init without fixing sqlc.
--
-- The split is not a workaround, it is the correct boundary: role and credential
-- bootstrap is not schema. init.sql is still the only DDL any environment
-- applies, and `scripts/check_schema_drift.py` still parses it as the single
-- source of relations. This file creates no table, view, index or policy — if
-- you are about to add one here, it belongs in init.sql instead.
--
-- ORDERING IS LOAD-BEARING. init.sql's `GRANT ... TO auditor_app, auditor_sys`
-- and its final self-check (`pg_roles WHERE rolname = 'auditor_app'`) both
-- require these roles to already exist. The `00-` prefix makes the postgres
-- image's /docker-entrypoint-initdb.d glob run this first; CI runs the two files
-- in the same order explicitly. Renaming this file so it sorts after init.sql
-- breaks database init — `scripts/check_bootstrap_split.py` fails if it does.
--
-- \getenv, NOT `printf '%s' "$APP_DB_PASSWORD"`. The backquote form makes psql
-- run a SHELL, and inside double quotes the shell still expands $(...) and
-- backticks — so a password containing either would have been EXECUTED as a
-- command during database init. \getenv (psql 14+, and the postgres:16 image
-- ships psql 16) reads the variable directly with no shell involved. This is a
-- real injection control, not a style choice; the guard asserts it stays.
--
-- The two \set lines are not redundant: \getenv leaves the psql variable
-- UNCHANGED when the environment variable is absent, so without a defined
-- default, :'app_pw' would be emitted literally and fail with a syntax error
-- instead of the actionable message the DO block below raises.
--
-- Passwords come from the environment, never from this file. Both compose (via
-- the postgres service env) and CI must export APP_DB_PASSWORD and
-- SYS_DB_PASSWORD or this script stops here on purpose.
\set app_pw ''
\set sys_pw ''
\getenv app_pw APP_DB_PASSWORD
\getenv sys_pw SYS_DB_PASSWORD
SELECT set_config('auditor.bootstrap_app_pw', :'app_pw', false);
SELECT set_config('auditor.bootstrap_sys_pw', :'sys_pw', false);

-- TWO roles, because the code has two legitimately different access patterns.
-- The full argument — which call sites need which, and why BYPASSRLS rather than
-- superuser — stays in init.sql above its GRANT block, next to the privileges it
-- justifies. In short: auditor_app is the per-request path and RLS applies to it;
-- auditor_sys is NOSUPERUSER but BYPASSRLS for pre-auth identity lookups and the
-- cross-firm background workers.
DO $bootstrap$
DECLARE
    app_pw text := current_setting('auditor.bootstrap_app_pw', true);
    sys_pw text := current_setting('auditor.bootstrap_sys_pw', true);
BEGIN
    IF app_pw IS NULL OR length(app_pw) < 16 THEN
        RAISE EXCEPTION 'APP_DB_PASSWORD is unset or shorter than 16 chars. '
            'Set it in .env (see .env.example) and re-create the postgres volume; '
            'the api connects as auditor_app and RLS depends on it.';
    END IF;
    IF sys_pw IS NULL OR length(sys_pw) < 16 THEN
        RAISE EXCEPTION 'SYS_DB_PASSWORD is unset or shorter than 16 chars. '
            'Set it in .env (see .env.example); auth and the background workers '
            'connect as auditor_sys.';
    END IF;
    IF app_pw = sys_pw THEN
        RAISE EXCEPTION 'APP_DB_PASSWORD and SYS_DB_PASSWORD must differ; they '
            'are the RLS-enforced and RLS-bypassing credentials respectively.';
    END IF;

    -- format(%L) quotes and escapes; the literal never appears in this file.
    EXECUTE format(
        'CREATE ROLE auditor_app LOGIN PASSWORD %L '
        'NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOINHERIT', app_pw);
    EXECUTE format(
        'CREATE ROLE auditor_sys LOGIN PASSWORD %L '
        'NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS NOINHERIT', sys_pw);
END
$bootstrap$;

-- Scrub the passwords out of the session before anything else runs. These are
-- session-local GUCs (set_config with is_local=false persists for the SESSION,
-- not the transaction), and the postgres entrypoint runs each initdb file in its
-- own psql connection — but init.sql's own connection must never be able to read
-- them back, so they are cleared here rather than relied on to expire.
SELECT set_config('auditor.bootstrap_app_pw', '', false);
SELECT set_config('auditor.bootstrap_sys_pw', '', false);
