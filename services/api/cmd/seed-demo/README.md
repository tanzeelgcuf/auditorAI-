# seed-demo — synthetic demo data generator

**WARNING: DEMO DATA ONLY.** This command inserts fabricated fixtures directly via
SQL, bypassing the real extraction/verification pipeline. **Never run it against a
production database or with real client documents.**

## Usage

```sh
go run ./cmd/seed-demo -db-url postgres://auditor:auditor@localhost:5432/ai_auditor?sslmode=disable
```

Flags:

| Flag      | Default     | Description                                   |
|-----------|-------------|-----------------------------------------------|
| `-db-url` | (required)  | PostgreSQL DSN (same schema as `infra/init.sql`) |
| `-books`  | `2`         | Number of client books to create (2-3)        |
| `-seed`   | `20240101`  | PRNG seed — reproducible output               |

## What it creates

- One demo firm + a `demo@ai-auditor.dev` staff user (fixed IDs, idempotent).
- 2-3 client books, each with one invoice CSV, one bank OFX, one GL CSV as
  `source_documents` (randomized names/dates via gofakeit).
- For each book: 7 planted 3-way groups (`source_documents` +
  `extracted_entities` + `reconciliation_groups` + `reconciliation_group_members`
  + `audit_findings`):
  - 3 exact matches (1:1:1, auto-linked, `info` findings)
  - 2 within-tolerance variances (auto-linked, `info` findings)
  - 2 genuine high-severity mismatches (`needs_review` groups, `high` findings)
- Prints a summary: firm name, books, entity counts, findings by severity.

Severity bands match `services/verification/decision-graphs/gl_reconciliation.json`
(info ≤ tolerance; low ≤ 10×; medium ≤ 100×; high > 100×).

The seeded data never touches real client info — all names, counterparties, and
amounts are synthetic. After the demo, drop the seed rows (or the demo firm) to
clean up.
