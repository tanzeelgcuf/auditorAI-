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
| TOTP 2FA (firm_admin mandatory) | 🟡 | `auth.HandleEnableTOTP`/`HandleVerifyTOTP` exist; enforcement gate in login pending wiring |
| Email verification | ✅ | `users.email_verified` + verification flow |
| Audit logging of sensitive access | ✅ | `internal/middleware/auditlog.go` → `access_log` table |

### CC6.6 — Key management / CC6.7 — Data loss prevention
| Control | Status | Evidence / Notes |
|---|---|---|
| Per-tenant KMS keys on S3 | ⬜ | Schema + endpoint exist; S3 SSE-KMS wiring pending |
| Backup / point-in-time restore | 🟡 | pgBackRest in docker-compose; tested restore procedure pending (go-live checklist item) |
| Retention lock / soft delete | ✅ | `source_documents.deleted_at` + `retention_locked_until` |

---

## 2. Availability

### CC7.1-7.3 — Monitoring, availability, capacity
| Control | Status | Evidence / Notes |
|---|---|---|
| Health/readiness endpoints | ✅ | `/healthz`, `/readyz` on all services |
| Graceful shutdown (SIGTERM drain) | ✅ | Go `http.Server.Shutdown`, Rust tokio signal |
| Circuit breakers on cross-service calls | 🟡 | gobreaker in `middleware.go`; per-service wiring in progress |
| Observability (Langfuse, GlitchTip, Jaeger) | 🟡 | docker-compose services present; instrumentation wiring in progress |
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
2. **Tested backup/restore**, including single-tenant restore isolation (CC7.1)
3. **ClamAV in upload pipeline** (CC6.10)
4. **Full observability instrumentation** — Langfuse traces on every LLM call, Jaeger end-to-end spans (CC7.1)
5. **Formal incident-response plan** + breach-notification flow (CC7.4 / legal)
6. **DPA + Privacy Policy** ready before first enterprise sales conversation
7. **Synthetic vs production data policy** — never test against real financial data (already convention in seed-demo; document it)

## Known gaps we are deliberately accepting for v1

- No formal SOC2 Type II audit booked yet (cost/benefit at <10 firms)
- KMS real crypto pending (schema + endpoint exist)
- Incident response documented as plan, not yet drilled

---
*Last updated: 2026-08-01. Owner: engineering. Reviewed by: (to be assigned when formal audit scoped).*
