---
name: scope-lock
description: Prevents v1 scope creep — flags any code expanding beyond 3-way reconciliation
---

# Scope Lock Skill

## v1 Scope (ONLY THESE)

1. **3-way reconciliation**: Invoice line items ↔ Bank transactions ↔ GL entries
2. **Full traceability**: Every figure → source doc + bbox + rule/calculation
3. **Multi-tenancy**: Firm → Client book hierarchy with RLS
4. **Single rule category**: "GL reconciliation" (sum of invoices vs bank vs GL within tolerance)
5. **Open-source OCR**: docTR/Surya only, no Textract in v1
6. **Zen Engine**: Decision graph for tolerance evaluation only
6. **Web app**: Upload, review queue, report viewer with PDF overlay
7. **Single-box deploy**: docker-compose, no K3s

## Explicitly OUT of v1 Scope (Blocked)

| Feature | Documented In | When to Revisit |
|---------|---------------|-----------------|
| SOX control testing | `03-ai-auditor-fintech.md` | v2 — after paying firms using core reconciliation |
| BSA-AML rule categories | `03-ai-auditor-fintech.md` | v2 — same |
| Mobile app (React Native) | `03-ai-auditor-fintech.md` Prompt 6 | v1.5 — when firm requests on-the-go approvals |
| Airbyte accounting connectors | `03-ai-auditor-fintech.md` Prompt 5b | v2 — when pilot client asks for direct QB/Xero sync |
| Natural-language query over findings | `03-ai-auditor-fintech.md` Prompt 5b | v1.5 — after RAG infrastructure proven |
| SOC2 Type II formal audit prep | `03-ai-auditor-fintech.md` Prompt 7 | v1.5 — when enterprise deal requires it |
| XBRL export | `03-ai-auditor-fintech.md` Additional Features | v2 — regulatory need only |
| Multi-format export (Excel) | `03-ai-auditor-fintech.md` Additional Features | v1.5 — when bookkeepers request it |

## Enforcement Rules

1. **New Rule Category**: Any code adding a `rule_id` other than "gl_reconciliation" in `services/verification` → BLOCK with explanation
2. **New Service**: Any new service directory under `services/` → BLOCK unless explicitly approved
3. **LLM Math**: Any arithmetic/comparison logic in `services/agent-runtime` → BLOCK immediately (violates architecture-guardrails too)
4. **New OCR Backend**: Textract implementation in `services/ingestion` → WARN (allowed but gated behind config, not default)
5. **Mobile Code**: Any `apps/mobile` directory creation → BLOCK
6. **Airbyte Integration**: Any Airbyte client code → BLOCK
7. **K3s/K8s Configs**: Any Kubernetes manifests beyond docker-compose → BLOCK

## Escape Hatch

If a genuine need arises (e.g., a paying pilot client requires a specific v2 feature), the human must:
1. Explicitly approve in the PR/commit message: `SCOPE_EXCEPTION: <reason>`
2. Update this skill's "OUT of v1 Scope" table with the new target version
3. The skill will then allow that specific addition

## Integration

- Works with `architecture-guardrails` (LLM boundary) and `traceability-guardrails` (citation requirement)
- All three skills load together for Auditor repo
- PR reviews should run all three skill checks