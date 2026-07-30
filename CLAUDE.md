# AI Auditor v1 — CLAUDE.md

## Core Non-Negotiable Rules

1. **LLM Boundary**: `services/agent-runtime` (LangGraph, Python) ONLY does extraction, classification, and semantic mapping. It NEVER performs arithmetic, variance calculation, or compliance rule evaluation. All financial math and rule evaluation lives exclusively in `services/verification` (Rust, deterministic).

2. **Multi-Tenancy**: Two-level hierarchy — Firm (tenant) → Client Books (sub-scope). RLS must enforce BOTH levels:
   - `app.current_firm` = firm_id from JWT
   - `app.assigned_books` = CSV of book_ids the user is assigned to (firm_admin gets all firm's books)
   - Every client-book-scoped table policy checks `client_book_id = ANY(app.assigned_books)`

3. **Traceability**: Every reported financial figure MUST carry a citation: `(source_document_id, page, bbox)` + the exact rule/calculation that produced it. No code path may skip this — even for "obviously correct" values.

4. **v1 Scope Lock**: ONLY 3-way reconciliation (invoice ↔ bank transaction ↔ GL entry) with full traceability. NO SOX/BSA-AML rule categories yet — those are documented v2 extensions. The `scope-lock` skill will flag any code expanding beyond this.

## Stack

| Layer | Tech |
|-------|------|
| Web | Next.js 14, TypeScript, Tailwind, shadcn/ui |
| API | Go (chi), sqlc, NATS JetStream |
| Ingestion | Rust (tonic gRPC), docTR Python sidecar |
| Verification | Rust (tonic gRPC), Zen Engine (gorules/zen), rust_decimal |
| Agent Runtime | Python, LangGraph |
| DB | PostgreSQL + PgBouncer, RLS enabled |
| Vector | pgvector (or Qdrant) |
| Infra | docker-compose (v1 launch), Terraform (future) |
| Observability | Langfuse, GlitchTip, OpenTelemetry/Jaeger |

## Key Paths

- `services/api` — Go orchestration, multi-tenancy, traceability matrix, MCP server
- `services/ingestion` — OCR pipeline, bbox capture, structured data parsers (OFX/CSV/XLSX)
- `services/verification` — Deterministic reconciliation engine on Zen Engine
- `services/agent-runtime` — LangGraph extraction/classification/linking agent
- `apps/web` — Next.js app: upload, review queue, report viewer with PDF overlay
- `packages/shared-types` — Generated TS types from Go/proto

## CI Requirements

- `go test ./...`, `cargo test` (both Rust services), `pnpm test`
- Trivy + Semgrep from day one
- No unwrap() outside tests in Rust; 100% branch coverage on verification

## Subagents

- `backend-agent` → services/api
- `ingestion-agent` → services/ingestion
- `verification-agent` → services/verification (strict: no unwrap, no float money, 100% branch coverage)
- `extraction-agent` → services/agent-runtime
- `web-agent` → apps/web
- `qa-agent` — testing, CI
- `security-agent` — RLS, multi-tenancy review