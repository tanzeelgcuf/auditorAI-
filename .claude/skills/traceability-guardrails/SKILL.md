---
name: traceability-guardrails
description: Enforces that every financial figure output carries a full source citation
---

# Traceability Guardrails Skill

## Core Rule

**Every function that outputs a calculated or derived financial figure MUST accept and propagate a `SourceCitation` struct containing:**

1. `source_document_id` — UUID of the originating document
2. `page_number` — Page within that document
3. `bbox` — Normalized bounding box (0-1 coordinates: x, y, width, height)
4. `rule_id` — The rule/calculation that produced this value (e.g., "gl_reconciliation")
5. `rule_version` — Version hash of the rule at evaluation time
6. `calculation` — Human-readable formula string (e.g., "sum(120000,34050)=154050, GL=154100, variance=50")

## Enforcement Points

### In Rust (`services/verification`)
- The `ReconciliationResult` gRPC response MUST include all citation fields
- The Zen Engine decision graph output MUST be wrapped with citation metadata
- Any internal function computing `variance_cents` or `severity` must thread the citation through

### In Go (`services/api`)
- `audit_findings` INSERT must include all citation fields from verification response
- Report generation must embed citations in PDF overlay data
- MCP tools returning findings must include citation data

### In Python (`services/agent-runtime`)
- Never outputs financial calculations — this is a guard against scope creep
- Cross-linking confidence scores are NOT financial figures — they're match probabilities
- If any code here attempts to compute variance/totals, flag immediately

### In TypeScript (`apps/web`)
- PDF viewer citation overlay reads from `/v1/reports/{id}/citation/{findingId}`
- Every displayed finding shows its citation trail

## Lint/Check Rules

1. **PR Check**: Any new function returning a monetary value (int64 cents, Decimal, etc.) without a `SourceCitation` parameter/return field → FAIL
2. **Database**: `audit_findings` table requires all citation columns NOT NULL
3. **gRPC**: `ReconciliationResult` message requires all citation fields
4. **Code Review**: Human reviewer must verify citation flow for any new calculation path

## Scope Lock Integration

This skill works with `scope-lock` — if a PR adds a new rule category (SOX, BSA-AML, etc.), the traceability requirement applies immediately. No exceptions for "internal" or "debug" calculations.