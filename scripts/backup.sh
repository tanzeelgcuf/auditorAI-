#!/usr/bin/env bash
#
# Automated Postgres backup for AI Auditor v1.
#
# Usage (from repo root):
#   scripts/backup.sh
#   BACKUP_DIR=/var/backups BACKUP_KEEP=30 scripts/backup.sh
#
# Behaviour:
#   - Dumps the `postgres` compose service DIRECTLY (never pgbouncer — pg_dump
#     over pgbouncer is unsupported/broken). Uses the container's own pg_dump,
#     so the tool version always matches the postgres:16 server.
#   - gzip-compressed plain-SQL dump written to $BACKUP_DIR (default
#     scripts/backups/) with a UTC timestamp filename.
#   - Rotates: keeps the $BACKUP_KEEP (default 14) most recent dumps, deletes
#     the rest. Idempotent — re-running never overwrites or duplicates.
#   - Exits non-zero with a clear message on any failure.
#
# SCHEDULING (production) — this script does not self-schedule. An operator
# must invoke it on a timer, e.g. cron daily:
#     0 3 * * * cd /path/to/ai-auditor && POSTGRES_PASSWORD=... BACKUP_DIR=/var/backups/ai-auditor BACKUP_KEEP=30 scripts/backup.sh
#
# ponytail: the "postgres running" gate uses `docker compose ps -q postgres`,
# which in a multi-stack host can match a DIFFERENT project's container also
# named "postgres" (false-pass). Single-stack v1 is safe; if two ai-auditor or
# generic postgres stacks ever share a host, pin the gate to this compose
# project's container (--project-name + container name) — see SOC2 go-live.
#   launchd/systemd timer is equivalent. A non-zero exit means a backup FAILED;
#   the scheduler should alert on it (see SOC2_READINESS.md go-live item).
#
# Env:
#   POSTGRES_PASSWORD  password for $POSTGRES_USER (default: devpassword, the
#                      compose default in .env.example)
#   BACKUP_DIR         dump directory (default: scripts/backups/)
#   BACKUP_KEEP        number of dumps to retain (default: 14)
#
# TESTED RESTORE (proven live: restored row count == original row count):
#   1) Create the restore database (only if it does not already exist):
#        docker compose -f infra/docker-compose.yml exec -T postgres \
#          createdb -U auditor <restore_db>
#   2) Restore the dump:
#        gunzip -c scripts/backups/<dump>.sql.gz | \
#          docker compose -f infra/docker-compose.yml exec -T postgres \
#            psql -U auditor -d <restore_db>
#   3) Verify (row count must match the source):
#        docker compose -f infra/docker-compose.yml exec -T postgres \
#          psql -U auditor -d <restore_db> -tAc "SELECT count(*) FROM <table>"
#
# Why via the container's psql and not a host psql: the image is the only
# guaranteed-version-matched client. Plain-SQL dump (-Fp) is deliberately
# portable across pg versions for restore.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/infra/docker-compose.yml"

DB_NAME="${POSTGRES_DB:-ai_auditor}"
DB_USER="${POSTGRES_USER:-auditor}"
DB_PASS="${POSTGRES_PASSWORD:-devpassword}"   # dev fallback = .env.example default
BACKUP_DIR="${BACKUP_DIR:-$REPO_ROOT/scripts/backups}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"

# Keep the $keep most recent .sql.gz dumps in $dir. Filenames carry UTC
# timestamps, so glob lexical order == chronological order (oldest first).
rotate_backups() {
  local dir="$1" keep="$2" i
  local -a files=("$dir"/*.sql.gz)
  [ -e "${files[0]:-}" ] || return 0   # no matches -> literal glob, bail out
  # Glob is oldest-first (UTC-timestamp filenames); delete oldest overflow.
  for (( i = 0; i < ${#files[@]} - keep; i++ )); do
    rm -f -- "${files[$i]}"
  done
}

main() {
  mkdir -p "$BACKUP_DIR"

  # Require the postgres container to be up. pg_dump over pgbouncer is
  # unsupported, so back up from the `postgres` service only.
  local cid
  if ! cid="$(docker compose -f "$COMPOSE_FILE" ps -q postgres 2>/dev/null)" || [ -z "$cid" ]; then
    echo "ERROR: postgres not running — start with docker compose -f $COMPOSE_FILE up -d postgres" >&2
    exit 1
  fi

  local ts out
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  out="$BACKUP_DIR/${DB_NAME}_${ts}.sql.gz"

  if ! docker compose -f "$COMPOSE_FILE" exec -T \
        -e PGPASSWORD="$DB_PASS" \
        postgres pg_dump -U "$DB_USER" -d "$DB_NAME" -Fp \
        | gzip > "$out"; then
    rm -f -- "$out"
    echo "ERROR: pg_dump failed — no backup written" >&2
    exit 1
  fi

  rotate_backups "$BACKUP_DIR" "$BACKUP_KEEP"

  echo "backup written: $out"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
