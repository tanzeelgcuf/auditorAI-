# AI Auditor v1

3-way reconciliation (invoice ↔ bank transaction ↔ GL entry) with full traceability.
Built for small/mid accounting firms and outsourced bookkeepers.

## Architecture

```
├── apps/web              Next.js 14 — upload, review queue, report viewer
├── services/
│   ├── api               Go — orchestration, multi-tenancy, auth, MCP tools
│   ├── ingestion         Rust + docTR — OCR pipeline, structured data parsers
│   ├── verification      Rust + Zen Engine — deterministic reconciliation math
│   └── agent-runtime     Python/LangGraph — entity extraction & cross-linking
├── packages/shared-types Generated TS types
├── infra/                docker-compose (v1), Terraform (future)
└── proto/                gRPC service definitions
```

## Core Design Rules

1. **LLM = extraction only.** No math, no rule evaluation — all financial calculations in `services/verification` (Rust, deterministic, 100% branch coverage).
2. **Full traceability.** Every figure carries `(source_doc, page, bbox) + rule + version`. No citation? Block.
3. **Multi-tenancy.** Two-level RLS: Firm → Client Book. Staff see only assigned books.
4. **v1 scope = reconciliation only.** No SOX, AML, mobile, Airbyte — those are v2.

## Quick Start

```bash
# Start dependencies (Postgres, NATS, Qdrant, docTR sidecar)
docker compose -f infra/docker-compose.dev.yml up

# Run migration
cd services/api && go run ./cmd/migrate

# Start API
go run ./cmd/server

# Start services (separate terminals)
cd services/verification && cargo run
cd services/agent-runtime && python -m main
```

## Stack

| Layer | Tech |
|-------|------|
| API | Go (chi), sqlc, NATS JetStream |
| Ingestion | Rust (tonic), docTR Python sidecar |
| Verification | Rust, Zen Engine, rust_decimal |
| Agent | Python, LangGraph, Claude |
| DB | PostgreSQL 16 + PgBouncer, RLS |
| Observability | Langfuse, GlitchTip, Jaeger |
| Infra | docker-compose (v1 target)