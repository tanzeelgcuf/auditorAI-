---
name: architecture-guardrails
description: Enforces Go/Rust split rules, naming conventions, and LLM boundary rules for Auditor
---

# Architecture Guardrails Skill

## Core Rules (Non-Negotiable)

### Go/Rust Split
- **Go** (`services/api`): Request/response, I/O-bound, business logic, auth, DB access, job scheduling, HTTP/gRPC APIs
- **Rust** (`services/ingestion`, `services/verification`): Tight-loop numeric/audio/streaming work, zero-GC latency requirements, deterministic math
- **Python** (`services/agent-runtime`): LLM calls, LangGraph agent orchestration

### LLM Boundary (ZERO TOLERANCE)
The LLM in `services/agent-runtime` may ONLY:
- Extract entities from OCR output
- Classify entity types (invoice/bank/GL)
- Semantic mapping/cross-linking
- Generate natural language summaries

The LLM must NEVER:
- Perform arithmetic (addition, subtraction, variance calculation)
- Evaluate compliance rules (SOX, BSA-AML, tax)
- Make financial judgments about "materiality" or "significance"
- Calculate totals, balances, or any monetary math

All arithmetic, variance calculation, and rule evaluation lives EXCLUSIVELY in `services/verification` (Rust, deterministic).

### Naming Conventions
- Go: `PascalCase` for types, `camelCase` for functions/variables, `UPPER_SNAKE_CASE` for constants
- Rust: `PascalCase` for types, `snake_case` for functions/variables, `SCREAMING_SNAKE_CASE` for constants
- Database: `snake_case` for tables/columns, singular table names preferred

### Error Handling
- Go: Return `error` as last return value; wrap with `fmt.Errorf("%w: context", err)`
- Rust: `Result<T, ServiceError>` everywhere; no `unwrap()`/`expect()` outside `#[cfg(test)]`
- Python: Raise typed exceptions; structured logging with `structlog`

### Traceability Requirement
Every function outputting a financial figure MUST accept and propagate a `SourceCitation`:
```go
type SourceCitation struct {
    SourceDocumentID uuid.UUID
    PageNumber       int
    BBox             BoundingBox
    RuleID           string
    RuleVersion      string
    Calculation      string
}
```

### Multi-Tenancy
- Every tenant-scoped table has RLS policy checking `app.assigned_books`
- Go middleware sets `app.current_firm` and `app.assigned_books` per request
- No cross-tenant queries allowed ever

## Enforcement
- This skill is consulted before writing cross-service code
- PRs violating these rules should be flagged automatically
- The `traceability-guardrails` skill enforces the citation requirement specifically