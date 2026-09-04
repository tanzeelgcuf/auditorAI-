# AI Auditor — SOC2 Type II Readiness

Status: **in progress** — roadmap, not a compliance claim. This document maps current
controls against the AICPA Trust Services Criteria (TSC). It is the internal tracking
doc for our eventual formal SOC2 Type II audit.

---

## Scope

- System: AI Auditor — 3-way reconciliation (invoice ↔ bank ↔ GL) SaaS
- In-scope services: `services/api` (Go), `services/ingestion` (Rust), `services/verification` (Rust), `services/agent-runtime` (Python), `apps/web` (Next.js)
- Data in scope: financial documents, extracted entities, reconciliation findings, firm/client-book metadata

---

## Control status legend

| Status | Meaning |
|--------|---------|
| ✅ Implemented | Control in production code, tested |
| 🟡 Partial | Wiring exists, real-world hardening pending |
| ⬜ Gap | Not yet built — roadmap item |

---

## 1. Security

### CC6.1 — Access to information is restricted by logical access controls
| Control | Status | Evidence / Notes |
|---|---|---|
| Row-level security (two-level: firm + client book) | ✅ | `infra/init.sql` — every tenant-scoped table has RLS policies on `app.current_firm` / `app.assigned_books` |
| RLS session-var injection middleware | ✅ | `internal/middleware/middleware.go` `RLSInjector` |
| Cross-tenant isolation tests | ✅ | `internal/middleware/security_test.go` (11 tests: cross-firm 404, cross-book 404, crafted injection attempts) |
| No-existence-leak on cross-tenant access | ✅ | Handlers return 404 (not 403) for foreign resources |
| JWT auth (Argon2 password hash, 15-min access / 7-day refresh) | ✅ | `internal/auth/auth.go` |

### CC6.2 — Prevent unauthorized access to assets
| Control | Status | Evidence / Notes |
|---|---|---|
| Per-firm encryption keys | 🟡 | `data_encryption_keys` table + `POST /v1/admin/rotate-keys` (migration `000002`). Real KMS integration (AWS KMS) pending — keys are refs, not embedded crypto |
| Secrets via env vars only | ✅ | `.env.example`; no secrets in code or images |
| Secret rotation endpoint | 🟡 | Implemented; rotation policy/automation pending |

### CC6.3 — Limit access to trusted subjects
| Control | Status | Evidence / Notes |
|---|---|---|
| Firm-admin vs staff roles | ✅ | `users.role` + `RequireRole` middleware |
| Book-assignment scoping | ✅ | `user_book_assignments`; firm_admin auto-gets all books |
| Optional segregation of duties (preparer ≠ reviewer) | ✅ | `client_books.require_separate_reviewer` (doc 10 §3) |

### CC6.4 — Segregation of duties / CC6.5 — User identification
| Control | Status | Evidence / Notes |
|---|---|---|
| Argon2id password hashing | ✅ | `auth.HashPassword` |
| TOTP 2FA — login enforcement | ✅ | `auth.CheckSecondFactor` is called by `HandleLogin`; a user with `totp_secret` set cannot obtain tokens without a valid code, and an accepted code cannot be reused inside its 90s validity (`totp_last_code`/`totp_last_used_at`). Unit-tested in `internal/auth/totp_test.go` (16 cases, no DB required). Wired 2026-09-04 — before that the column was written by `/totp/verify` and read by nothing. |
| TOTP 2FA — enrollment ceremony | ✅ | `/v1/totp/enable` persists `totp_pending_secret`; `/v1/totp/verify` validates against the STORED secret and promotes it, stamping `totp_enabled_at`. Both routes are behind `Authenticator`. Before 2026-09-04 verify validated the code against a secret supplied in the same request body. |
| TOTP 2FA — mandatory for firm_admin | ⬜ | NOT implemented. Nothing forces a firm_admin to enroll; the factor is enforced only for accounts that have enrolled. This row is the remaining gap in the "firm_admin mandatory" claim earlier versions of this file made. |
| TOTP 2FA — recovery codes | ⬜ | NOT implemented, deliberately deferred. A user who loses their authenticator needs an operator to clear `users.totp_secret`; there is no self-service path and no break-glass code. |
| TOTP 2FA — enrollment UI | ⬜ | No client calls `/v1/totp/enable` or `/v1/totp/verify` (`grep -rn totp apps/` returns only the login-form code field). Enrollment is API-only today. |
| Email verification | ✅ | `users.email_verified` + verification flow |
| Brute-force ceiling — per source address | ✅ | Per-IP token bucket (`internal/middleware/ratelimit.go`) on `/v1/auth/*`, `/v1/portal/login`, `/v1/totp/*`, uploads and admin key ops. **Was bypassable until 2026-09-04**: `clientIP` read the leftmost `X-Forwarded-For` element — the one the client writes — and preferred it over the peer address, and `chimiddleware.RealIP` poisoned the peer address from the same headers, so one header per request gave a caller unlimited private buckets. Measured against a burst-1 bucket: 1000/1000 forged requests admitted before, 1 after. Now trusted-proxy aware (`clientip.go`, `TRUSTED_PROXY_CIDRS`, empty = ignore headers). |
| Brute-force ceiling — per account (lockout) | 🟡 | Tiered auto-expiring lockout on `/v1/auth/login`, added 2026-09-04. `internal/auth/lockout.go` is the only place the policy lives: 5 consecutive failures → 1 min, 10 → 5 min, 15 → 15 min, 20+ → 30 min cap; **every** failure at or above the threshold re-locks (not only tier boundaries), the counter clears after a 60-min idle window or on a full success, and a failure arriving *inside* a window neither extends it nor increments. Wrong password **and** wrong TOTP each spend one attempt, so an attacker holding the password gains no fresh budget by switching to the six digits; a *missing* code (`ErrTOTPRequired`) is user error and does not count. Read-check-increment holds a `SELECT … FOR UPDATE` row lock and the increment is committed on its own transaction (`HandleLogin` has a deferred `Rollback`, so a 401-path write is discarded by default). Measured ceiling for an attacker who never pauses: **64 attempts per 24 h against one account** — 10⁶/64 ≈ 15,600 days to walk the TOTP space. `HandleResetPassword` clears all three columns so a locked-out user who resets is not still refused. **Amber, not green, for one reason: no test has ever executed this path against a database.** `internal/auth/lockout_test.go` (13 cases) covers the arithmetic as pure functions and `internal/auth/login_lockout_test.go` (12 cases) pins the *ordering* in `HandleLogin` by asserting on the source text — because the three mistakes that would silently remove the ceiling (verifying the password while locked, which is a password oracle that never consumes the counter; clearing the counter before the second factor, which pins it at 1; and leaving the increment on the cancellable request context, which lets an attacker who hangs up avoid being counted at all — the last of those was a real defect in this code, found the same day it was written) are ordering mistakes no pure-function test can reach. Neither file has been compiled — `go` is absent from the environment that wrote them; the assertions were transcribed into a Python mirror and run instead, which is evidence about the assertions, not about the runtime. Green requires the DB-backed case in the `DATABASE_URL_TEST` suite. Also still open: `/v1/portal/login` and `/v1/totp/*` have no per-account counter, only the per-IP layer above. |
| Login timing side-channel | ⬜ | Pre-existing and deliberately left in place. An unknown email is rejected immediately; a known one costs ~50 ms of Argon2id, so response time distinguishes them. The usual fix — hashing a dummy password on the unknown-email path — makes every unauthenticated request cost a full Argon2id, i.e. turns login into CPU amplification, which is a worse trade at this scale. Recorded here rather than silently accepted. |
| Audit logging of sensitive access | ✅ | `internal/middleware/auditlog.go` → `access_log` table. **Was cancellable by the recorded party until 2026-09-04**: all 12 call sites pass `r.Context()`, which dies with the client's socket, and the INSERT error degrades to a `slog.Warn` — so hanging up immediately after a request dropped its own audit row while the action itself stood, already committed. Now `context.WithoutCancel` + a 5s deadline. Two sibling instances of the same class were found and fixed the same day (`auth.persistLoginFailure`, `humanoverride.LogConfigChange` → `config_change_log`), plus one already fixed earlier (`middleware.ReleaseRLSConn`, where the leak was tenant GUCs surviving onto the next request to acquire that pooled connection). Standing guard: `scripts/check_cancellable_audit_writes.py` in CI, which both flags new instances and fails by name if any of the four fixes is reverted. |
| Source IP recorded on audited access | ⬜ | `access_log` has no IP column (`user_id`, `client_book_id`, `action`, `resource_id`, `occurred_at`). An audit product should be able to answer "from where"; today it cannot. |

### CC6.6 — Key management / CC6.7 — Data loss prevention
| Control | Status | Evidence / Notes |
|---|---|---|
| Per-tenant KMS keys on S3 | ⬜ | Schema + endpoint exist; S3 SSE-KMS wiring pending |
| Backup / restore | 🟡 | `scripts/backup.sh` pg_dump snapshot + retention; restore proven (tested live, restored row count == source); point-in-time restore (WAL archiving) NOT wired — separate go-live item |
| Retention lock / soft delete | ✅ | `source_documents.deleted_at` + `retention_locked_until` |

---

## 2. Availability

### CC7.1-7.3 — Monitoring, availability, capacity
| Control | Status | Evidence / Notes |
|---|---|---|
| Health/readiness endpoints | ✅ | `/healthz`, `/readyz` on all services |
| Graceful shutdown (SIGTERM drain) | ✅ | Go `http.Server.Shutdown`, Rust tokio signal |
| Circuit breakers on cross-service calls | 🟡 | gobreaker in `middleware.go`; per-service wiring in progress |
| Observability (Langfuse, GlitchTip, Jaeger) | 🟡 | Sentry/GlitchTip error reporting wired in all 4 services (Go panic wrapper + capture, Python `capture_exception`, Rust panic hooks both services); Langfuse extraction traces wired; Jaeger end-to-end spans pending |
| NATS JetStream queueing for bursty ingestion | ✅ | `pipeline` package + docker-compose NATS |
| Verification-engine hard-fail on error (never silent wrong number) | ✅ | `#![deny(clippy::unwrap_used)]`, `Result<T, ServiceError>` everywhere in verification |

---

## 3. Confidentiality

### CC6.8-6.10 — Confidential data
| Control | Status | Evidence / Notes |
|---|---|---|
| RLS confines confidential data to authorized books | ✅ | Above |
| No PII/financial data in logs | ✅ | structlog/slog conventions; access_log stores IDs, not content |
| 25MB upload cap + type allowlist | ✅ | `documents.go` |
| ClamAV scanning on upload | ⬜ | Not yet wired — go-live checklist item |

---

## 4. Processing Integrity

### CC6.11 — Process data accurately
| Control | Status | Evidence / Notes |
|---|---|---|
| Deterministic verification engine (zero LLM involvement) | ✅ | `services/verification` — rust_decimal, 66 tests, 100% branch target |
| Traceability matrix: every figure → (doc, page, bbox, rule, version) | ✅ | `GET /v1/reports/{id}/citation/{findingId}` |
| Rule-version hashing on findings | ✅ | `rule_version` SHA-256 prefix stored per finding |
| Statement self-consistency + trial-balance checks | ✅ | Schema columns + verification checks |
| Human-review queue for low-confidence links | ✅ | `needs_review` routing, review-queue UI |

---

## 5. Privacy

### CC7.4 — Privacy principles
| Control | Status | Evidence / Notes |
|---|---|---|
| Retention lock (soft delete + 7yr) | ✅ | `retention_locked_until` |
| GDPR right-to-erasure path | ⬜ | Soft-delete only; hard-erase flow pending legal confirmation |
| Privacy policy / DPA | ⬜ | Legal docs pending (go-live checklist) |

---

## Roadmap (priority order)

1. **AWS KMS integration** for per-firm data keys + S3 SSE-KMS (CC6.6) — before first enterprise deal
2. **DB-backed auth integration tests** in the `DATABASE_URL_TEST` suite (CC6.5) — the per-account lockout row above is amber only for want of these. Two cases carry it: a locked account must be refused *without* its password being checked, and a wrong TOTP code carrying the correct password must still increment. Both are ordering properties currently pinned only by assertions on source text.
3. **Tested backup/restore**, including single-tenant restore isolation (CC7.1)
4. **ClamAV in upload pipeline** (CC6.10)
5. **Observability residual** — GlitchTip/Sentry error reporting DONE (all 4 services); remaining: Langfuse traces on every LLM call, Jaeger end-to-end spans, AND capture non-panic error returns (gRPC `Status::internal`, graph-load failures) in Rust + Go — currently only panics + agent-runtime exceptions reach GlitchTip (CC7.1)
6. **Formal incident-response plan** + breach-notification flow (CC7.4 / legal)
7. **DPA + Privacy Policy** ready before first enterprise sales conversation
8. **Synthetic vs production data policy** — never test against real financial data (already convention in seed-demo; document it)

## Known gaps we are deliberately accepting for v1

- No formal SOC2 Type II audit booked yet (cost/benefit at <10 firms)
- KMS real crypto pending (schema + endpoint exist)
- Incident response documented as plan, not yet drilled

---
*Last updated: 2026-09-04. Owner: engineering. Reviewed by: (to be assigned when formal audit scoped).*
