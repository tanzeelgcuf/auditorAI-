# AI Auditor v1 — CLAUDE.md

Repo root: `~/Desktop/auditor/auditorgit`. The numbered spec docs (00, 03–14)
that the Claude Desktop Project instructions refer to are **not in this repo** —
do not cite them as though you had read them, and do not assume a design
question is already answered there.

Session reports live at the workspace root next to the repo:
`../AUDIT_2026-09-02.md`, `../SESSION_2026-09-04.md`,
`../SESSION_2026-09-04_pipeline.md`.

## Core Non-Negotiable Rules

1. **LLM Boundary**: `services/agent-runtime` (LangGraph, Python) ONLY does extraction, classification, and semantic mapping. It NEVER performs arithmetic, variance calculation, or compliance rule evaluation. All financial math and rule evaluation lives exclusively in `services/verification` (Rust, deterministic).
   - Known live tension: `services/agent-runtime/graph/link.py` pass 1 auto-links at confidence 1.0 using its own `_amounts_match`. That is a comparison, not a conversion, but it is still a money decision made outside Rust. Open, documented, not yet moved.

2. **Multi-Tenancy**: Two-level hierarchy — Firm (tenant) → Client Books (sub-scope). RLS must enforce BOTH levels:
   - `app.current_firm` = firm_id from JWT
   - `app.assigned_books` = CSV of book_ids the user is assigned to (firm_admin gets all firm's books)
   - Every client-book-scoped table policy checks `client_book_id = ANY(app.assigned_books)`
   - RLS is only real if the connecting role is neither the table owner nor `BYPASSRLS`. See **Database roles** below; this was inert for the whole project until 2026-09-02.

3. **Traceability**: Every reported financial figure MUST carry a citation: `(source_document_id, page, bbox)` + the exact rule/calculation that produced it. No code path may skip this — even for "obviously correct" values.
   - **A recorded rule identifier is not provenance unless that rule produced the number.** Until 2026-09-04 every reconciliation result carried `rule_version` (a SHA-256 prefix of the decision-graph JSON) while `RuleEngine::evaluate` ignored the graph completely and used a hardcoded severity ladder. Editing a firm's tolerance bands changed the recorded `rule_version` and changed nothing about the answer. Fixed; see `services/verification/src/zen/mod.rs` header.

4. **v1 Scope Lock**: ONLY 3-way reconciliation (invoice ↔ bank transaction ↔ GL entry) with full traceability. NO SOX/BSA-AML rule categories yet — those are documented v2 extensions. The `scope-lock` skill will flag any code expanding beyond this.

5. **A NATS message is acked only after its work is durable.** Not in a `defer`,
   not in a `finally`, and not on an error path. Every consumer in this repo
   violated this at some depth, and the failure is silent by construction: the
   event is destroyed, so nothing retries it and nothing reports it. See
   **Pipeline durability** below before touching a consumer.

## Pipeline durability

Three consumers, all fixed 2026-09-04, all found by grepping for the first one's
shape (`defer .*\.Ack()`, `finally:`, `_, _ = ...Publish`):

| Consumer | Was | Consequence when it failed |
|----------|-----|----------------------------|
| `internal/pipeline/coordinator.go` | acked before persisting entities | document marked `done`, no entities, nothing extracted |
| `internal/pipeline/verify_worker.go` | `defer msg.Ack()`, 4 error paths logged and returned | group linked, **no finding ever written** — reads as "reconciled cleanly" |
| `agent-runtime/main.py` | acked in a `finally`, plus 3 internal swallows | LLM ran, groups produced, write failed, event gone |

Two rules the fixes established, both of which cost a bug to learn:

- **A retry is only a fix if the write is idempotent.** Adding Nak to
  `coordinator.persistEntities` turned a partial insert (row 40 of 100) into 39
  duplicated entities on redelivery. Duplicated legs change a group's totals, so
  that trade swaps a *missing* finding for a *confidently wrong* one, which is
  worse in an audit product. `persistEntities` is now one transaction, guarded by
  a presence count taken after `SELECT ... FOR UPDATE` on the `source_documents`
  row. `extracted_entities` has **no unique constraint** (only
  `idx_extracted_entities_ref/_book/_doc`, all non-unique), so `ON CONFLICT` is
  not available — and it skips rather than deletes, because
  `extracted_entities.id` is referenced by `reconciliation_group_members` and
  `corrects_entity_id`.
- **Fix the callee, not just the call site.** `main.py` was made to let
  `persist_groups` raise — and it could not: `persist_groups` caught every
  per-group exception and returned the success count, so 50/50 failures returned
  `0`, logged `written=0`, and acked. The call-site fix was vacuous for as long as
  that stood. Its docstring now records that the book-wide link pass is safe to
  retry only because `HandleGetPendingEntities` filters
  `AND id NOT IN (SELECT extracted_entity_id FROM reconciliation_group_members)`
  (`internal/mcp/mcp.go:87`) — that FILTER is the idempotency guard, **not** the
  `UNIQUE (reconciliation_group_id, extracted_entity_id)` constraint, which is
  per-group and does not stop an entity joining a second group. Relax the filter
  and link retries start duplicating groups.

`MAX_DELIVERY_ATTEMPTS` is 5 in both halves (`main.py`, `coordinator.go`) so the
pipeline gives up at the same count on either side. `nats-py` is not installed in
the environment this was written in, so `msg.metadata.num_delivered` is read
defensively and degrades to `1` — biasing toward extra retries rather than loss.

A **WorkQueue** stream deletes a message only on ack, so a subject with no
consumer grows forever. `DOCUMENTS` carried two such subjects
(`ingestion.completed`, produced at `services/ingestion/src/grpc/mod.rs:204`, and
`document.processing.failed`); they now live on `PIPELINE_EVENTS`
(LimitsPolicy, 7d, 100k). **`CreateStream` does not rewrite an existing stream's
config** — an already-deployed `DOCUMENTS` keeps the old subject list until it is
updated or deleted while drained. See the MIGRATION NOTE in `pipeline.go`.

## Stack

| Layer | Tech |
|-------|------|
| Web | Next.js 14, TypeScript, Tailwind, shadcn/ui |
| API | Go (chi), sqlc, NATS JetStream |
| Ingestion | Rust (tonic gRPC), docTR Python sidecar |
| Verification | Rust (tonic gRPC), rust_decimal, **Zen-format decision graphs evaluated in-crate** (`src/zen`) |
| Agent Runtime | Python, LangGraph |
| DB | PostgreSQL + PgBouncer, RLS enabled |
| Vector | pgvector (or Qdrant) |
| Infra | docker-compose (v1 launch), Terraform (future) |
| Observability | Langfuse, GlitchTip, OpenTelemetry/Jaeger — `GLITCHTIP_DSN` (Sentry-compatible) is consumed by ALL FOUR services: api (Go sentry-go), agent-runtime (Python sentry-sdk), ingestion + verification (Rust sentry crate). Empty = error reporting OFF everywhere (no-op). Defined in `.env.example`. |

> The Verification row said "Zen Engine (gorules/zen)" until 2026-09-04. There is
> no zen dependency of any kind: `grep -n zen services/verification/Cargo.toml`
> returns no match, and `grep -rn 'zen_engine\|zen-engine\|ZenEngine' src` returns
> nothing. The graph FILE FORMAT is Zen-compatible so the files stay portable;
> evaluation is ~600 lines of in-crate Rust. Say "Zen-format graphs, evaluated
> in-crate" — the shorter claim was what made an unused-graph bug invisible.

## Key Paths

- `services/api` — Go orchestration, multi-tenancy, traceability matrix, MCP server
- `services/ingestion` — OCR pipeline, bbox capture, structured data parsers (OFX/CSV/XLSX)
- `services/verification` — deterministic reconciliation engine; the ONLY place money math happens
  - `src/decimal_math` — rust_decimal sums/variance, property-tested with proptest
  - `src/zen` — loads `decision-graphs/*.json`, compiles each band at load, validates that the bands tile `[0..∞)` exactly once, evaluates first-match. No fallback severity: an uninterpretable graph fails at LOAD so the process refuses to start.
  - `decision-graphs/gl_reconciliation.json` — the shipped policy: `[0..t]`→info, `(t..t*10]`→low, `(t*10..t*100]`→medium, `(t*100..]`→high, where `t` = per-request `tolerance_cents`
- `services/agent-runtime` — LangGraph extraction/classification/linking agent
- `apps/web` — Next.js app: upload, review queue, report viewer with PDF overlay
- `packages/shared-types` — Generated TS types from Go/proto

## Database

Single baseline: **`infra/init.sql`**. `db/migrations/` was deleted 2026-09-02 —
only init.sql was ever applied, and the two had drifted to the point where 6
relations and 10 columns that live code queried did not exist in the deployed
database. Change the schema by editing init.sql; `scripts/check_schema_drift.py`
fails CI if code references a relation or column it does not define.

Three roles, and the split is load-bearing:

| Role | Posture | Used by |
|------|---------|---------|
| `auditor` | owner | migrations, CI fixtures, `DATABASE_URL_TEST_OWNER` |
| `auditor_app` | NOSUPERUSER, NOBYPASSRLS, **non-owner** | the app pool (`DATABASE_URL`) — RLS actually applies |
| `auditor_sys` | BYPASSRLS | pre-auth identity lookups, cross-firm workers (`SYS_DATABASE_URL`) |

`init.sql` reads `APP_DB_PASSWORD` / `SYS_DB_PASSWORD` via `\getenv` and RAISEs if
either is unset, under 16 chars, or equal to the other — so loading the schema
without them set aborts before creating a single table.

`internal/middleware/security_test.go:117` (`assertPoolsDiffer`) fails the suite if
`DATABASE_URL_TEST` and `DATABASE_URL_TEST_OWNER` resolve to the same enforcement
posture, so collapsing back to one DSN goes red instead of quietly passing.

**Which pool a service gets** (wired in `cmd/server/main.go`): `sysPool` only when
the work cannot be scoped to one firm — `auth` (all `/v1/auth` routes are public,
so no `app.current_firm` exists yet, and login must find `firm_id` *from* the
submitted email while signup INSERTs the `firms` row a policy would need to
already exist), `SeedTemplates`, `notify.Run`, the coordinator, the verify worker,
`webhooks` (its DB writes run *after* outbound HTTP retry backoffs, so they can
outlive the request), `billing`'s Stripe webhook, and `portal`'s pre-auth invite
lookup. Everything a request handler touches uses `pool`.

`SYS_DATABASE_URL` unset is **fatal at boot**, not a fallback to `pool`: the
`firms` policy (`init.sql:466`) calls `current_setting` *without* `missing_ok`, so
an unset GUC RAISEs rather than returning NULL — the fallback would be a wall of
500s across login, signup, reset and the portal instead of one message.
`assertRolePosture` (`cmd/server/rolecheck.go`) asks Postgres what it actually
enforces per connection (`row_security_active`, `rolbypassrls`) and refuses to
start on a mismatch; `seed-demo` refuses to run when row security *is* active,
because it creates the firm it then writes into.

`/v1/webhooks/stripe` is registered **public**, and deliberately so — it lived
inside the `Authenticator` group until 2026-09-04, and Stripe cannot present a
JWT, so every delivery was 401'd before the handler ran and billing state diverged
from Stripe with no error on our side. The handler authenticates its own caller
(secret unset → 503; `webhook.ConstructEvent` verifies the HMAC over the raw body
→ 400) and is rate-limited. Do not "fix" it back into the auth group.

## CI — `.github/workflows/ci.yml`, 8 jobs

`go` · `rust-ingestion` · `rust-verification` · `Schema Drift Guard` · `python` ·
`web` · `security` · `docker`

Per-language gates, stated accurately:

- **Go**: real Postgres 16 service container with `infra/init.sql` loaded, `sqlc compile`, golangci-lint, `go test ./... -race` against four distinct DSNs, coverage uploaded to codecov (**no threshold configured**).
- **Rust** (both crates): `cargo fmt --check`, `cargo clippy --all-targets -- -D warnings`, `cargo test --all-targets`, `cargo build --release --locked`. Ingestion is built WITHOUT `--all-features` on purpose — the `enhance` feature gates deliberately-inert stages the shipped image does not enable.
- **Python**: entrypoint import check (`import ollama_adapter, mcp_client, graph.graph_def`) separately from `pytest tests -q`, because a missing runtime dep shows up in the import chain while every test still passes.
- **Web**: `npm ci`, `npm run lint`, `npx tsc --noEmit`, `npm run build`, then asserts `.next/standalone/server.js` exists.
- **Docker**: builds all 6 images with the same context/`-f` split as `infra/docker-compose.yml`, asserts binaries and the decision graph are actually inside the images, and `docker compose config -q` on both compose files.

Three **static guards** — they exist because each proves something about code that
no test executes, and each was verified in both directions (clean on the current
tree, red when the original bug is reintroduced) before being wired in:

| Script | Job | Catches |
|--------|-----|---------|
| `scripts/check_schema_drift.py` | Schema Drift Guard | SQL in Go/Python referencing relations or columns `init.sql` does not define |
| `scripts/check_storage_key_orphans.py` | Schema Drift Guard | a file that writes a `storage_key` without writing the bytes (`PutObject`) or verifying them (`ObjectExists`) |
| `scripts/check_amount_parity.py` | python | the Rust money parser and its Python mirror disagreeing (they once read `"1250"` as $1,250.00 and $12.50) |

Lint rules that bite in non-obvious ways:

- **No `unwrap()` outside tests in Rust.** Enforced by `#![deny(clippy::unwrap_used)]` at both crate roots. Verified 2026-09-04: **0 production unwraps in either crate.**
- That deny reaches into `#[cfg(test)]` modules and CI passes `--all-targets`, so the 64 test-module unwraps were deny-level errors. Both crates now ship a `clippy.toml` with `allow-unwrap-in-tests` / `allow-expect-in-tests`. Do not "simplify" those files away.
- `cargo fmt --check` rejects tabs, trailing whitespace, and over-width **code** lines (rustfmt leaves comments alone, and does not split string literals).
- `clippy -D warnings` rejects `format!` with no arguments (`useless_format`).
- Renaming a CI job's **display name** silently drops any branch-protection required check keyed on it. `Schema Drift Guard` keeps its name even though it now hosts two guards.

## What is NOT true (read this before repeating a claim from this file)

This project's most expensive bug class has been confident documentation. Claims
removed from earlier versions of this file because they were checked and found
false:

- ~~"Zen Engine (gorules/zen)"~~ — no such dependency (see the Stack note above).
- ~~"`pnpm test`"~~ — `grep -rn pnpm .github/workflows/ apps/web/package.json` returns nothing. The web job uses `npm ci`, and `apps/web/package.json` declares only `dev`, `build`, `start`, `lint` — **there is no web test script to run.** The web app has zero automated tests; lint + `tsc --noEmit` + a successful build is its entire gate.
- ~~"100% branch coverage on verification"~~ — **no Rust coverage gate exists.** The only coverage step in the whole file is `codecov/codecov-action@v4` at line 100, inside the Go job, with no threshold set. `grep -rn 'tarpaulin\|llvm-cov\|grcov'` returns nothing. Treat this as an aspiration, not a gate.

**Nothing in this repo has been compiled or executed in the sessions that wrote
most of it.** `cargo`, `rustc`, `go`, `gofmt`, `psql` and `docker` are all absent
from the environment those sessions ran in (verified with `command -v`). Structural
checkers were used instead and are explicitly not compilers — they cannot see type
errors, trait bounds, moved values, missing imports, or match exhaustiveness. The
first real CI run should be expected to surface compile errors, and that is not
evidence the design is wrong.

## Verification standard for this repo

- A green suite that never exercised the real path is not evidence. Two examples already found here: the entire gRPC severity suite ran against a **zero-rule** engine via a `test_engine()` helper built from `{"nodes":[],"edges":[]}`, and `test_pilot_fixtures.py` parses CSV/OFX in Python instead of calling the Rust parser it is meant to prove.
- Before calling something a model/tool limitation, run an isolation test that changes one variable and confirms the result changes. The regression test for the graph bug is exactly this shape: same input, two graphs differing only in thresholds, asserted to produce **different** severities.
- When you fix one instance of a bug class, sweep for the others before closing it. Every guard in the table above found a second or third instance after the first.
- Report the literal command output, not a summary of what you expect it to say.

## Subagents

`.claude/agents/` — note that some of these files assert facts about the code that
are stale; they are inputs to be checked, not authority.

- `backend-agent` → services/api (its reference to `db/migrations/` is dead)
- `ingestion-agent` → services/ingestion
- `verification-agent` → services/verification (strict: no unwrap, no float money; its "Zen Engine" and "100% branch coverage" claims are both false as implemented)
- `extraction-agent` → services/agent-runtime
- `web-agent` → apps/web
- `qa-agent` — testing, CI (names Grype/promptfoo/Ragas; none are wired)
- `security-agent` — RLS, multi-tenancy review



