# AI Auditor v1 — CLAUDE.md

Repo root: `~/Desktop/auditor/auditorgit`. The numbered spec docs (00, 03–14)
that the Claude Desktop Project instructions refer to are **not in this repo** —
do not cite them as though you had read them, and do not assume a design
question is already answered there.

Session reports live at the workspace root next to the repo:
`../AUDIT_2026-09-02.md`, `../SESSION_2026-09-04.md`,
`../SESSION_2026-09-04_pipeline.md`, `../SESSION_2026-09-04_totp.md`,
`../SESSION_2026-09-04_clientip.md`, `../SESSION_2026-09-04_disposition.md`.

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

4. **v1 Scope Lock**: ONLY 3-way reconciliation (invoice ↔ bank transaction ↔ GL
   entry) with full traceability. NO SOX/BSA-AML rule categories yet — those are
   documented v2 extensions. The `scope-lock` skill will flag any code expanding
   beyond this.
   - **The lock governs PRODUCT SURFACE, not security controls.** Auth hardening —
     MFA, session handling, rate limits, RLS, audit logging — is baseline for any
     multi-tenant product that touches a client's books, not a feature added to
     v1's scope. Decided 2026-09-04, recorded here so the scope-lock check does
     not flag the next piece of auth work as an expansion. What the lock still
     forbids: new *reconciliation* categories, new rule domains, and anything
     that makes this the system of record.

5. **A NATS message is acked only after its work is durable.** Not in a `defer`,
   not in a `finally`, and not on an error path. Every consumer in this repo
   violated this at some depth, and the failure is silent by construction: the
   event is destroyed, so nothing retries it and nothing reports it. See
   **Pipeline durability** below before touching a consumer.

6. **One definition per context key, and it lives at the bottom of the import
   order.** The four request-scoped identity keys (`UserIDKey`, `FirmIDKey`,
   `AssignedBooksKey`, `RoleKey`) are declared in `internal/auth/context.go` and
   *aliased* in `internal/middleware`. Never read a context value with an untyped
   string literal — `ctx.Value("user_id")` does not match a
   `ContextKey("user_id")` write and returns nil forever, with no error anywhere.
   See **Second factor** below for what that cost.

7. **A request header is never an identity.** `X-Forwarded-For`, `X-Real-IP`,
   `True-Client-IP` and friends are attacker-controlled unless the machine that
   opened the connection is one we chose to trust, and `X-Forwarded-For` is read
   **right to left** because conforming proxies append. Client-IP resolution lives
   in exactly one place, `internal/middleware/clientip.go`, gated on
   `TRUSTED_PROXY_CIDRS`, which is **empty by default and means trust nothing**.
   Do not reintroduce `chimiddleware.RealIP` — it rewrites `RemoteAddr` from those
   headers with no trusted-proxy check. See **Client IP and rate limits** below.

8. **Confidence is not tolerance.** `link_confidence` answers "are these records
   the same transaction" (amount 0.5, date 0.2, counterparty 0.3). Whether the
   amounts *reconcile* is a separate question that only `services/verification`
   answers. A 2¢ gap on an $899 invoice scores 0.99998 on the first and fails the
   second. Never gate a disposition on a confidence score, and never treat
   `is_exact` as a tolerance verdict. See **Group disposition** below.

9. **The deterministic tier disposes.** A verdict from `services/verification` is
   not an annotation. Wherever it is obtained it must reach
   `reconciliation_groups.status`, in the same transaction as the finding, and
   **downgrade only** — `exceeds_tolerance == false` never promotes
   `needs_review` to `auto_linked`, because a group is in review for reasons that
   tier cannot see. Every status write carries `AND status = 'auto_linked'`.
   Failure to verify fails **closed**.

10. **Presence is membership, not a non-zero total.** A leg is present iff it has
    ≥1 member. An invoice plus its full credit note nets to zero and is still
    present, with total 0. This must agree in three places at once —
    `verify_worker.go`'s `BOOL_OR(m.role=…)`, `link.py`'s `is_exact`, and the
    `has_invoice/has_bank/has_gl` flags on the gRPC request, since
    `grpc/mod.rs:130-141` builds each leg only `if req.has_X` and proto3 defaults
    an unset bool to **false**. An unset flag does not mean "unknown"; it means
    the money tier never compares that leg and returns "clean" for everything.

11. **A locked account is never told it is locked, and a correct password never
    clears the counter.** Both halves are ordering rules in `HandleLogin`, both
    read as improvements in review, and either one alone removes the per-account
    ceiling entirely:
    - Verifying the password while `IsLocked` is true — even just to give the
      user a clearer message — is an unlimited password-correctness oracle that
      **does not consume the counter**. The locked branch returns before
      `VerifyPassword` and answers with the same `invalidCredentials(w)` as a bad
      password. A lockout that responds differently from a wrong password is also
      a user-enumeration oracle.
    - The counter is cleared only on the path that **issues tokens**, after the
      second factor. Every TOTP guess in an attack carries the *correct*
      password, so a reset on password-correct would pin the counter at 1 and the
      10⁶ ceiling would not exist. A 403 (email unverified) neither increments
      nor clears: no guess failed, and zeroing there is a free reset for anyone
      holding the password.

    Corollaries, each pinned by a test: the read-check-increment holds a
    `FOR UPDATE` row lock; the increment **commits** (`HandleLogin` has a deferred
    `Rollback`, so a 401-path write is discarded by default); a failure arriving
    *during* a window does not extend it; and `ErrTOTPRequired` — a missing code,
    not a wrong one — does not count.

12. **The party being recorded does not hold the cancel button.** `r.Context()`
    is cancelled the instant the client's socket closes. A database write on it
    whose error is *swallowed into a log line* can therefore be deleted by the
    caller — they hang up, the row is never written, nothing reports a failure
    that mattered, and the action being recorded stands because it already
    committed. A write whose error **is** returned is not in this class: losing it
    fails the request, which is visible.

    The fix, identical in all four places it was needed:

    ```go
    ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancel()
    ```

    `WithoutCancel` keeps the ctx **values** — `RecordAccess` needs them for
    `DB(ctx, db)` to find the RLS-wired request connection — and drops only
    cancellation; the deadline replaces the one it dropped. The pooled DB
    connection has nothing to do with the client's socket, so it stays usable.

    The four: `middleware.ReleaseRLSConn` (a `RESET` that no-op'd on aborted
    requests, leaking tenant GUCs into the next request on that connection),
    `auth.persistLoginFailure` (the increment that *is* rule 11's ceiling),
    `middleware.RecordAccess` (all 12 `access_log` call sites) and
    `humanoverride.LogConfigChange` (`config_change_log`). Three were
    pre-existing; the class is now a CI guard,
    `scripts/check_cancellable_audit_writes.py`.

    **The asymmetry is deliberate, so do not "make it consistent".** A *success*
    commit must stay cancellable: losing it issues no tokens, burns no TOTP code
    and clears no counter, which is fail-**closed**. Only writes whose loss is
    fail-**open** get the strip. Cancellation is also the shutdown signal for
    background loops and NATS consumers, so stripping it there would keep a
    draining process writing after it was told to stop — the three allowlisted
    sites in that guard are allowlisted for exactly that reason.

13. **The source address is resolved once, at the top, and every audited write
    reads that one answer.** `access_log`, `config_change_log` and
    `period_reopen_log` each carry `source_ip INET`, and the value comes from
    `middleware.SourceIP`, mounted **directly below
    `middleware.RealIP(trustedProxies)`** in `main.go`, which copies the by-then
    rewritten `RemoteAddr` onto the request context. `auth.SourceIPFrom(ctx)` /
    `middleware.GetSourceIP(ctx)` is how a writer gets it — never a new
    parameter, and never a second read of `X-Forwarded-For`. Twelve
    `RecordAccess` call sites each re-deriving "which IP is this?" is precisely
    the shape of the bypass rule 7 exists to prevent, and it would also let the
    audit trail and the rate limiter disagree about who called.

    Three ways this breaks silently, all pinned by
    `scripts/check_audit_ip_arity.py` because none of them is visible at
    runtime:
    - **Mount order.** `SourceIP` above `RealIP` reads the untouched
      `RemoteAddr` and records the *proxy* on every request behind a trusted
      proxy. No error, no log line, a permanently wrong audit trail. It is a
      source-order property, so no unit test on either function alone can see
      it; `sourceip_test.go` carries a negative control that builds the chain
      wrongly and asserts the wrong answer, because an ordering claim nobody
      has ever seen fail is not evidence.
    - **Positional binds.** pgx binds *and* scans by position. A column added
      without its argument errors at runtime — and `RecordAccess` /
      `LogConfigChange` swallow that error into a `slog.Warn` (rule 12), so the
      row simply vanishes. Two columns of the same type **transposed** raises
      nothing at all and writes a confidently wrong row, which for this product
      is worse than an empty one. The guard cross-matches column names against
      argument names and counts select-list length against Scan-target length.
    - **`continue` on a Scan error.** `HandleConfigHistory` skips rows whose
      Scan fails, so a select list one item longer than its Scan list returns
      `{"items": []}` for every book, forever, with a 200.

    The column is **nullable on purpose**. `SourceIPFrom` returns `""` for a
    background worker, a CLI, a unit test or an unparseable `RemoteAddr`, and
    that is stored as SQL NULL via `NULLIF($n,'')::inet` — mirroring the proven
    in-repo `NULLIF($n,'')::uuid` idiom, which also sidesteps any pgx `inet`
    codec question; the read side is `COALESCE(source_ip::text,'')`. NOT NULL
    would mean discarding an entire audit row to protect one field, and a row
    that admits it does not know beats one asserting a bogus address.

14. **A write against an RLS table runs on the request's primed connection, not
    on the pool.** `middleware.DB(ctx, db)` returns the connection
    `RLSInjector` set `app.current_firm` and `app.assigned_books` on; a bare
    `db.Exec(...)` takes an arbitrary pooled connection with neither GUC set.
    All 30 policies call `current_setting('app.…')` with **no `missing_ok`** and
    there is no database- or role-level default, so such a write either raises
    on the unset parameter or — on a connection recycled after `RESET` — tests
    against `''` and violates the policy. It cannot succeed.

    Survivable where the error is returned: the request 500s and someone
    notices. Invisible where the error is swallowed, which is the intersection
    with rule 12 and how `humanoverride.LogConfigChange` came to be a write
    that could only fail, into a `slog.Warn`. The most likely reading is that
    `config_change_log` never received a row in any deployment and
    `GET /v1/books/{bookId}/config-history` answered `{"items": []}` with a 200
    the whole time. **Not runtime-verified** — no Postgres in the environment
    that found it, so this is reasoned from the policy text plus documented GUC
    semantics, and it stays amber in `SOC2_READINESS.md` until the
    `DATABASE_URL_TEST` suite lands a row.

    `check_audit_ip_arity.py` pins `LogConfigChange` and `RecordAccess` to
    `middleware.DB(ctx, db)` **by name**, because reverting that one-line fix
    was tested against every other guard in the repo and produced exit 0
    everywhere. `sysPool` callers (`auth.go` ×8, `webhooks.go` ×3) are correct
    by design — they must see across firms — and the ~9 remaining suspect
    app-pool handler sites are listed in `SOC2_READINESS.md` roadmap item 9.
    Before adding any statement against an RLS table, answer two questions:
    which pool does it run on, and is its error returned?

## Group disposition

Until 2026-09-04 the Rust verdict was computed, recorded, and then thrown away at
every point where it could have changed a group's disposition. Five instances,
one bug class — a group whose amounts do not reconcile was published as
reconciled, carrying an open over-tolerance finding no human would ever be shown.
Measured before the fix: **139 of 600 randomised books' auto_linked groups were
over tolerance by Rust's own arithmetic.**

| Where | What it did |
|-------|-------------|
| `pipeline/verify_worker.go` | Success path wrote the finding and stopped; `review.go:71` selects the queue on `status`, so the group never appeared in it. |
| `link.py is_exact` | Compared each leg against `present_totals[0]` only — a **star**. Two legs one tolerance off the invoice in opposite directions are 2× tolerance apart and passed. Rust takes the **max of all three pairwise** variances. |
| `link.py` presence | `total != 0`, so a zero-net invoice leg was invisible and the group auto-linked on the other two. |
| `link.py _score_group` | `abs(vi - vj)` on **signed** totals. 3-way groups carry opposite signs by convention, so `amount_score` clamped to 0.0, the path collapsed to `0.2·date + 0.3·cp`, and a near-miss landed exactly on `review_floor` — a fuzzy counterparty then routed a real discrepancy to **neither queue**. |
| `mcp.go HandleCreateEntityLink` | Took `req.Status` from the caller. A group created `'confirmed'` is immune to the downgrade *because* that UPDATE is guarded on `auto_linked`. |
| `graph_def.py _verify_node` | Never sent the presence flags (so every leg arrived absent and no group could be flagged), never applied the result, and left the group `auto_linked` on exception. **Unreachable in production** — `main.py:212` passes no `verification_client` — fixed and pinned anyway. |
| `infra/init.sql` | `status DEFAULT 'auto_linked'`. A default disposition must mean "nobody decided". Now `'needs_review'`. |

The window in the star-comparison bug **scales with the book's tolerance**, not
with the matcher's uncertainty: at `tolerance = 2500` it auto-linked a $50.00
gap at confidence 1.0. That is why this class is worse than a low-confidence
mismatch — it is silent and it grows with a customer-configurable number.

Tests: `services/agent-runtime/tests/test_link_tolerance.py` (8),
`tests/test_verify_node.py` (10), `internal/mcp/mcp_test.go`,
`internal/pipeline/verify_worker_test.go`. Read the last one's header before
trusting it — it asserts on source **text**, because a `jetstream.Msg` cannot be
built outside a live connection, and the behavioural equivalent belongs in the
`DATABASE_URL_TEST` suite and does not exist yet.

## Client IP and rate limits

Until 2026-09-04 every per-IP control in `services/api` was bypassable with one
header, and the bug was in two places at once — the pattern this repo keeps
hitting, where a control looks present because two halves each assume the other
validated something.

| Where | What it did |
|-------|-------------|
| `main.go` `r.Use(chimiddleware.RealIP)` | rewrote `r.RemoteAddr` from `True-Client-IP` / `X-Real-IP` / `X-Forwarded-For`, unconditionally |
| `ratelimit.go` `clientIP` | read the **leftmost** `X-Forwarded-For` element and **preferred it over** `RemoteAddr` |

Leftmost is the element the *client* writes. So `X-Forwarded-For: <anything new>`
on each request minted a fresh token bucket, and the 5 req/s ceiling on
`/v1/auth/login`, `/v1/portal/login`, `/v1/totp/verify`, uploads and admin key
rotation was not a ceiling. `infra/docker-compose.yml` publishes the api as
`8080:8080` and no service carries traefik labels, so the header arrived from the
internet untouched — nothing was sanitising it. Measured against a burst-1
bucket: **1000 of 1000 forged requests admitted before the fix, 1 after**, with
1000 distinct buckets minted from a single peer.

A green test asserted the vulnerable behaviour as correct
(`TestRateLimitHonorsXForwardedFor`: "same forwarded IP → same bucket"). It was
replaced, not deleted quietly — `TestRateLimitIgnoresSpoofedXForwardedFor` names
what it supersedes and why.

Two consequences worth keeping in mind:

- The bucket map's key used to be an unvalidated caller-supplied string, so the
  file's own comment claiming "an attacker can't exhaust memory by rotating IPs"
  was false — no IPs needed rotating. Keys are now canonicalised addresses
  (`::ffff:1.2.3.4` and `1.2.3.4` collapse to one bucket) and unparseable peers
  share a single `"unresolved"` key.
- This limiter is per **source address**, so on its own it bounds an attack by the
  addresses available to it, not by attempts against one account. `/v1/auth/login`
  gained a second, per-account ceiling on 2026-09-04 — see **Per-account lockout**
  below. `/v1/portal/login` and `/v1/totp/*` still have only this per-IP layer.
- Since 2026-09-05 the three audit tables record that same resolved address in
  `source_ip`, so a per-IP decision is auditable after the fact and cannot
  disagree with what the limiter bucketed — `middleware.SourceIP` is mounted
  directly below `RealIP` and both read one resolution. **That adjacency is
  load-bearing and is checked in CI**; see rule 13. Nothing yet *reads*
  `access_log` through the API, so the address is stored and surfaced to nobody
  (`SOC2_READINESS.md` roadmap item 10).

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

## Second factor (TOTP)

Wired 2026-09-04. Before that date the feature was **write-only**: `/totp/verify`
wrote `users.totp_secret` and no code path ever read it, so an account with 2FA
"enabled" logged in with a password alone. Both shipped clients already collected
and sent `totp_code`; the server had no field to decode it into.

Three separate defects, only one of which was the one originally logged:

| Defect | Effect |
|--------|--------|
| `loginRequest` had no `TOTPCode`, `HandleLogin` never selected `totp_secret` | the factor did not exist at login; enabling it changed nothing |
| `HandleVerifyTOTP` validated `code` against `secret` **from the same request body** | proved only that the caller could run a TOTP library; a client could enroll a secret the user's authenticator had never seen |
| both handlers read `r.Context().Value("user_id")` — untyped `string` key vs `contextKey` write — while mounted in the **public** `/v1/auth` group | 401 for every caller, which is the only reason the two defects above were never live |

That last row is why fixing the context key *alone* would have been worse than
leaving it: it would have converted a visibly broken endpoint into a working
endpoint that enrolls unverified secrets and a login that ignores them.

How it works now:

- `auth.CheckSecondFactor(state, submitted, now)` in `internal/auth/totp.go` is a
  pure function — no DB, no clock, no HTTP — and is the only place the
  accept/reject decision is made. `HandleLogin` does the I/O around it.
  `totp_secret == ""` means *not enrolled*, so every pre-existing user row (NULL)
  keeps logging in unchanged.
- **Enrollment is a two-step ceremony.** `/v1/totp/enable` generates a secret and
  stores it in `totp_pending_secret`; `/v1/totp/verify` validates the submitted
  code against **that stored secret**, then promotes it to `totp_secret` in one
  `UPDATE ... WHERE totp_pending_secret IS NOT NULL` and stamps `totp_enabled_at`.
  The request body of `/totp/verify` carries no secret at all any more.
- **Routes moved** from `/v1/auth/totp/*` (public group) to `/v1/totp/*` behind
  `Authenticator`, in their own small group rather than the big protected one:
  both handlers query through the auth service's `sysPool` and never touch the
  RLS-scoped pool, so putting them in the main group would make `RLSInjector`
  check out a connection and set GUCs for a request that cannot use them. Safe
  because identity comes from the verified JWT and every statement is scoped
  `WHERE id = <that user>`. No client called the old paths (`grep -rn totp apps/`
  → only the login form's code field), so nothing working was broken.
- **Single-use codes.** `totp.Validate` accepts the current 30s step ±1, so one
  code is good for ~90s. `totp_last_code`/`totp_last_used_at` remember the last
  accepted code for 120s and reject it, so a code seen once — screenshot,
  shoulder, phished form — cannot be spent again inside its own window. The
  "still spent" test is `now < last_used + 120s`, which stays fail-closed if the
  database clock reads ahead of the API's.

Not implemented, and named so it is not mistaken for done: **recovery/backup
codes** (losing the authenticator needs an operator to clear `totp_secret`),
**mandatory enrollment for firm_admin** (the factor is enforced only for accounts
that chose to enroll), and **any enrollment UI**. See `SOC2_READINESS.md` CC6.5,
where each is its own row rather than one amber cell.

Tests: `internal/auth/totp_test.go`, 16 table cases plus 4 standalone, no database
required. The valid codes are produced by a hand-rolled RFC 6238 implementation
(stdlib `hmac`/`sha1`) rather than by the `otp` package, so "valid code accepted"
cross-checks two implementations of the spec instead of one library agreeing with
itself.

## Per-account lockout

Added 2026-09-04, the same day the `X-Forwarded-For` bypass was closed. Until
then the **only** brute-force control in the service was the per-IP token bucket,
which is keyed on the source address and therefore bounds nothing against a
distributed attempt on one known email. Combined with the second factor shipping
that morning, that left 10⁶ — a small number — reachable by anyone who already
had the password.

Policy, and it lives in exactly one place, `internal/auth/lockout.go`:

| Consecutive failures | Locked for |
|---|---|
| 1-4 | not locked |
| 5-9 | 1 minute |
| 10-14 | 5 minutes |
| 15-19 | 15 minutes |
| 20+ | 30 minutes (cap) |

The ladder is evaluated on **every** failure at or above the threshold, not only
on the boundaries — otherwise attempts 6-9 are free guesses handed out the moment
the 60-second lock expires. `LockoutResetWindow` is **60m and must stay strictly
greater than the 30m cap**: if they were equal, the lock expiring and the counter
zeroing would coincide and an attacker parked on the top tier would collect 5
fresh attempts per 30 minutes instead of 1.

Measured ceiling, not estimated: **64 attempts per 24 hours** against one account,
with a gap of exactly 30 minutes from attempt 20 onward. The first draft of the
test asserted "roughly 40-60" and was wrong; running the arithmetic is what caught
it. Both a wrong password and a wrong-or-replayed TOTP code spend the same
counter, so switching factors buys nothing.

`HandleResetPassword` clears `failed_login_attempts`, `locked_until` and
`last_failed_login_at`. Without that a user who got locked out, assumed they had
forgotten the password, and reset it stays refused with "invalid email or
password" while holding a password they know is correct.

Not covered, and named rather than implied: the per-IP limiter is still the only
control on `/v1/portal/login` and `/v1/totp/*` (the portal path is a 256-bit
invite token, so it is not a guessable surface; the TOTP endpoints sit behind a
verified JWT). There is also a **pre-existing timing side-channel** — an unknown
email rejects immediately, a known one costs ~50 ms of Argon2id — left in place
deliberately: a dummy-hash fix turns the login endpoint into a CPU-amplification
DoS, and the lockout adds noise to that channel rather than widening it.

Tests: `internal/auth/lockout_test.go` (13, pure) and
`internal/auth/login_lockout_test.go` (12, source-invariant). Read the second
one's header before trusting it — it asserts on the **text** of `auth.go` because
`HandleLogin` needs a live Postgres, and the behavioural equivalent belongs in the
`DATABASE_URL_TEST` suite and does not exist yet. Neither file has been compiled;
both were mirrored in Python and run, including five mutants of the current
`auth.go` in which only the ordering, the Scan arity, or the cancellation strip is
wrong.

One defect in this control was found *the same day, while writing its own session
report*: `persistLoginFailure` performed its UPDATE and its Commit on
`r.Context()`, so a client who hung up immediately cancelled the increment, the
error went to `slog.Error`, and the ceiling above silently stopped existing. See
rule 12 — the strip is now pinned by invariant 9 in `login_lockout_test.go` and by
`scripts/check_cancellable_audit_writes.py`. It was fail-open, not an oracle:
`persistLoginFailure` runs *before* `invalidCredentials(w)`, so aborting to kill
the increment forfeits the answer too.


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
the work cannot be scoped to one firm — `auth` (every `/v1/auth` route is public,
so no `app.current_firm` exists yet, and login must find `firm_id` *from* the
submitted email while signup INSERTs the `firms` row a policy would need to
already exist; the two `/v1/totp/*` routes ARE authenticated but share the same
service and pool, which is safe because each statement is scoped `WHERE id =`
the JWT's own user), `SeedTemplates`, `notify.Run`, the coordinator, the verify worker,
`webhooks` (its DB writes run *after* outbound HTTP retry backoffs, so they can
outlive the request), `billing`'s Stripe webhook, and `portal`'s pre-auth invite
lookup. Everything a request handler touches uses `pool`.

Getting `pool` is only half of it: a statement against an RLS table must also run
on the **request's** connection via `middleware.DB(ctx, db)`, or the GUCs
`RLSInjector` set are not on it and the statement cannot succeed. See rule 14 —
this was a live bug in `LogConfigChange`, and roughly nine app-pool handler sites
have not been cleared yet.

The three audit tables (`access_log`, `config_change_log`, `period_reopen_log`)
each carry `source_ip INET`, **nullable**, plus a partial index
`idx_access_log_source_ip ON access_log(source_ip, occurred_at DESC) WHERE
source_ip IS NOT NULL`. Written as `NULLIF($n,'')::inet`, read as
`COALESCE(source_ip::text,'')`. Rule 13 has the reasoning; the short version is
that an audit row admitting it does not know where a request came from is worth
more than one asserting a bogus address, and NOT NULL would have thrown away the
whole row to protect that one field.

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

Five **static guards** — they exist because each proves something about code that
no test executes, and each was verified in both directions (clean on the current
tree, red when the original bug is reintroduced) before being wired in:

| Script | Job | Catches |
|--------|-----|---------|
| `scripts/check_schema_drift.py` | Schema Drift Guard | SQL in Go/Python referencing relations or columns `init.sql` does not define |
| `scripts/check_storage_key_orphans.py` | Schema Drift Guard | a file that writes a `storage_key` without writing the bytes (`PutObject`) or verifying them (`ObjectExists`) |
| `scripts/check_cancellable_audit_writes.py` | Schema Drift Guard | rule 12 — an audit/security write on a request context whose error is only logged, plus a positive check that the four known fixes still carry `context.WithoutCancel` |
| `scripts/check_audit_ip_arity.py` | Schema Drift Guard | rule 13 and rule 14 — `source_ip` dropped from an audit INSERT, a column/placeholder/argument arity mismatch, two same-type columns **transposed** (which raises nothing and writes a confidently wrong row), a select list longer than its Scan list, `SourceIP` mounted above `RealIP` or unmounted, and either audit writer reverted from `middleware.DB(ctx, db)` to the raw pool. 11 plausible mutants were run against it, 11 caught, each by its own invariant. |
| `scripts/check_amount_parity.py` | python | the Rust money parser and its Python mirror disagreeing (they once read `"1250"` as $1,250.00 and $12.50) |

Every one of these fails by **name** as well as by pattern — the positive half
means silently reverting a fix goes red, which is not hypothetical: reverting
`LogConfigChange` to `db.Exec` was tested against the whole guard suite before
that half existed and produced **exit 0 everywhere**. A guard that only detects
*new* instances of a class lets the fixed instance rot back.

Lint rules that bite in non-obvious ways:

- **No `unwrap()` outside tests in Rust.** Enforced by `#![deny(clippy::unwrap_used)]` at both crate roots. Verified 2026-09-04: **0 production unwraps in either crate.**
- That deny reaches into `#[cfg(test)]` modules and CI passes `--all-targets`, so the 64 test-module unwraps were deny-level errors. Both crates now ship a `clippy.toml` with `allow-unwrap-in-tests` / `allow-expect-in-tests`. Do not "simplify" those files away.
- `cargo fmt --check` rejects tabs, trailing whitespace, and over-width **code** lines (rustfmt leaves comments alone, and does not split string literals).
- `clippy -D warnings` rejects `format!` with no arguments (`useless_format`).
- Renaming a CI job's **display name** silently drops any branch-protection required check keyed on it. `Schema Drift Guard` keeps its name even though it now hosts three guards.

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

Specific claims in this file that are **reasoned, not observed**, listed so nobody
upgrades them by repetition:

- **`config_change_log` has never received a row** (rule 14). Derived from the
  policy text in `init.sql` plus documented `current_setting` semantics with no
  `missing_ok`. There is no Postgres here to confirm it. What *is* observed is
  that the write ran on the raw pool and its error went to a `slog.Warn`.
- **The three `_test.go` files written for rules 11 and 13** —
  `internal/auth/login_lockout_test.go`, `internal/auth/lockout_test.go` and
  `internal/middleware/sourceip_test.go` — have never been compiled. Their
  expected values were re-derived by Python ports (the lockout arithmetic, and a
  15/15 port of `clientip.go`), which is evidence about the **assertions**, not
  about the Go runtime. Precedent for why that distinction matters: the first
  draft of the lockout test asserted a 24-hour ceiling of "40–60 attempts" when
  the measured figure was 64.
- **Every ✅ in `SOC2_READINESS.md`** rests on source reading, non-DB unit tests
  and the static guards above. That combination has caught real defects — the
  header bypass, the cancellable writes, the unprimed-pool INSERT — and is still
  not the same claim as "behaves this way in production".

## Verification standard for this repo

- A green suite that never exercised the real path is not evidence. Two examples already found here: the entire gRPC severity suite ran against a **zero-rule** engine via a `test_engine()` helper built from `{"nodes":[],"edges":[]}`, and `test_pilot_fixtures.py` parses CSV/OFX in Python instead of calling the Rust parser it is meant to prove.
- **A test that also passes against the pre-fix code proves nothing.** Method used on 2026-09-04: `git archive HEAD` into a scratch tree, copy the new tests in, re-run. 18 new tests → 12 fail there, and the 6 that pass on both sides are over-correction guards. One test was caught this way *after* it was written and looked green: `test_zero_net_invoice_leg_is_compared_not_ignored` passed on both trees, because `build_candidate_groups` never admits a `50000` invoice into a group whose bank leg is `-89900`, so the assertion loop never ran. Rewritten to call `score_and_route` directly.
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



