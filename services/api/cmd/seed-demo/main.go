// WARNING: DEMO DATA ONLY — NEVER run this against a production database or
// with real client documents. This command generates fully synthetic fixtures
// and inserts them directly via SQL, BYPASSING the real extraction/verification
// pipeline. It exists solely to make sales demos and manual QA fast.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AmountDiscrepancy describes one deliberately planted 3-way discrepancy.
type AmountDiscrepancy struct {
	InvoiceCents   int
	BankCents      int
	GLCents        int
	ToleranceCents int
}

// Severity of the planted mismatch, per services/verification's tolerance bands:
// info  = within tolerance
// high  = variance > tolerance*100
// medium = tolerance*10 < variance <= tolerance*100
// low   = tolerance < variance <= tolerance*10
func (d AmountDiscrepancy) Severity() string {
	max := d.InvoiceCents
	if d.BankCents > max {
		max = d.BankCents
	}
	if d.GLCents > max {
		max = d.GLCents
	}
	variance := max - d.InvoiceCents
	if d.BankCents < max {
		variance = max - d.BankCents
	}
	if d.GLCents < max {
		variance = max - d.GLCents
	}
	t := d.ToleranceCents
	switch {
	case variance <= t:
		return "info"
	case variance <= t*10:
		return "low"
	case variance <= t*100:
		return "medium"
	default:
		return "high"
	}
}

func main() {
	dbURL := flag.String("db-url", "", "postgres DSN (required)")
	books := flag.Int("books", 2, "number of client books to create (2-3)")
	seed := flag.Int64("seed", 20240101, "PRNG seed for reproducibility")
	flag.Parse()

	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "seed-demo: -db-url is required")
		fmt.Fprintln(os.Stderr, "  example: go run ./cmd/seed-demo -db-url 'postgres://auditor:auditor@localhost:5432/ai_auditor?sslmode=disable'")
		os.Exit(2)
	}
	if *books < 2 || *books > 3 {
		fmt.Fprintln(os.Stderr, "seed-demo: -books must be 2 or 3")
		os.Exit(2)
	}

	gofakeit.Seed(*seed)
	rng := rand.New(rand.NewSource(*seed))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-demo: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "seed-demo: ping: %v\n", err)
		os.Exit(1)
	}

	firmName := gofakeit.Company()
	firmID := uuid.MustParse("00000000-0000-0000-0000-00000000f1aa")
	_, err = pool.Exec(ctx, `
		INSERT INTO firms (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`,
		firmID, firmName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-demo: firm: %v\n", err)
		os.Exit(1)
	}

	// Demo staff user (idempotent) — required FK target for source_documents.
	demoUserID := uuid.MustParse("00000000-0000-0000-0000-00000000d3b0")
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, firm_id, email, password_hash, role)
		VALUES ($1, $2, 'demo@ai-auditor.dev', '!demo-not-a-real-hash!', 'firm_admin')
		ON CONFLICT (email) DO NOTHING`,
		demoUserID, firmID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-demo: demo user: %v\n", err)
		os.Exit(1)
	}

	booksSummary := make([]bookSummary, 0, *books)

	for b := 0; b < *books; b++ {
		book := seedBook(ctx, pool, firmID, demoUserID, rng, b)
		booksSummary = append(booksSummary, book)
	}

	fmt.Println("=== Seed Demo Complete ===")
	fmt.Printf("Firm: %s (id %s)\n", firmName, firmID)
	fmt.Printf("Client books: %d\n", len(booksSummary))
	for _, b := range booksSummary {
		fmt.Printf("  - %s: %d entities; exact matches=%d within-tolerance=%d mismatches=%d; findings %v\n",
			b.name, b.entityCount, b.exactMatches, b.withinTol, b.mismatches, b.bySeverity)
	}
	fmt.Println("REMINDER: these are SYNTHETIC fixtures for demos. Never point this at production.")
}

type bookSummary struct {
	name         string
	entityCount  int
	bySeverity   map[string]int
	exactMatches int
	withinTol    int
	mismatches   int
}

type seededEntity struct {
	id           uuid.UUID
	docID        uuid.UUID
	docType      string
	entityType   string
	subtype      string
	amountCents  int
	debitCredit  string
	txnDate      time.Time
	counterparty string
	description  string
	glCode       string
	role         string // invoice | bank | gl
}

type seededGroup struct {
	id          uuid.UUID
	invID       uuid.UUID
	bankID      uuid.UUID
	glID        uuid.UUID
	confidence  float64
	status      string
	discrepancy AmountDiscrepancy
}

func seedBook(
	ctx context.Context,
	pool *pgxpool.Pool,
	firmID uuid.UUID,
	userID uuid.UUID,
	rng *rand.Rand,
	idx int,
) bookSummary {
	bookName := gofakeit.Company()
	bookID := uuid.New()
	tolerance := 100 // $1.00 in cents

	_, err := pool.Exec(ctx, `
		INSERT INTO client_books (id, firm_id, client_name, base_currency, reconciliation_tolerance_cents)
		VALUES ($1, $2, $3, 'USD', $4)`,
		bookID, firmID, bookName, tolerance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-demo: book: %v\n", err)
		os.Exit(1)
	}

	// --- planted discrepancy plan: exact / within-tolerance / high mismatch ---
	var discrepancies []AmountDiscrepancy
	for i := 0; i < 3; i++ {
		base := 10000 + rng.Intn(90000) // $100-$1000
		disc := rng.Intn(30) - 15       // ±$0.15 → exact (within $1 tolerance)
		discrepancies = append(discrepancies, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base + disc,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	for i := 0; i < 2; i++ {
		base := 20000 + rng.Intn(80000)
		disc := 1 + rng.Intn(tolerance) // within tolerance → info finding
		discrepancies = append(discrepancies, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	for i := 0; i < 2; i++ {
		base := 50000 + rng.Intn(200000)
		disc := tolerance*100 + rng.Intn(50000) // > 100x tolerance → high severity
		discrepancies = append(discrepancies, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	// deterministic shuffle
	rng.Shuffle(len(discrepancies), func(i, j int) {
		discrepancies[i], discrepancies[j] = discrepancies[j], discrepancies[i]
	})

	// three documents (one per type) covering all planted rows
	docs := []struct {
		docType string
		storage string
	}{
		{"invoice", "demo/" + bookID.String() + "/invoice.csv"},
		{"bank_statement", "demo/" + bookID.String() + "/bank.ofx"},
		{"gl_export", "demo/" + bookID.String() + "/gl.csv"},
	}
	docIDs := make([]uuid.UUID, 3)
	for i, d := range docs {
		docIDs[i] = uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO source_documents (id, client_book_id, filename, doc_type, storage_key, content_hash, uploaded_by, ocr_status, page_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'done', 1)`,
			docIDs[i], bookID, d.storage, d.docType, d.storage,
			fmt.Sprintf("sha256:demo:%s", d.storage), userID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "seed-demo: source_document: %v\n", err)
			os.Exit(1)
		}
	}

	entities := make([]seededEntity, 0, len(discrepancies)*3)
	var groups []seededGroup
	bySeverity := map[string]int{}
	exact, withinTol, mismatches := 0, 0, 0

	for _, d := range discrepancies {
		cp := gofakeit.Company()
		dt := gofakeit.DateRange(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC))
		inv := seededEntity{
			id: uuid.New(), docID: docIDs[0], docType: "invoice",
			entityType: "invoice_line_item", subtype: "standard",
			amountCents: d.InvoiceCents, debitCredit: "debit",
			txnDate: dt, counterparty: cp, description: "Professional services (demo)",
			glCode: "4000", role: "invoice",
		}
		bank := seededEntity{
			id: uuid.New(), docID: docIDs[1], docType: "bank_statement",
			entityType: "bank_transaction", subtype: "standard",
			amountCents: d.BankCents, debitCredit: "debit",
			txnDate: dt, counterparty: cp, description: "ACH DEBIT (demo)",
			glCode: "", role: "bank",
		}
		gl := seededEntity{
			id: uuid.New(), docID: docIDs[2], docType: "gl_export",
			entityType: "gl_entry", subtype: "standard",
			amountCents: d.GLCents, debitCredit: "debit",
			txnDate: dt, counterparty: cp, description: "GL EXPENSE (demo)",
			glCode: "4000", role: "gl",
		}
		entities = append(entities, inv, bank, gl)

		// Group status + confidence derived from planted discrepancy severity.
		group := seededGroup{
			id: uuid.New(), invID: inv.id, bankID: bank.id, glID: gl.id,
			discrepancy: d,
		}
		sev := d.Severity()
		bySeverity[sev]++
		switch sev {
		case "info":
			group.confidence, group.status = 1.0, "auto_linked"
			exact++
		case "low":
			group.confidence, group.status = 0.9, "auto_linked"
			withinTol++
		default:
			group.confidence, group.status = 0.4, "needs_review"
			mismatches++
		}
		groups = append(groups, group)
	}

	for _, e := range entities {
		var dc *string
		if e.debitCredit != "" {
			dc = &e.debitCredit
		}
		var glCode *string
		if e.glCode != "" {
			glCode = &e.glCode
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO extracted_entities
				(id, client_book_id, source_document_id, entity_type, entity_subtype, amount_cents,
				 currency, debit_or_credit, transaction_date, counterparty, description,
				 gl_account_code, page_number, bbox, extraction_confidence, source_format)
			VALUES ($1,$2,$3,$4,$5,$6,'USD',$7,$8,$9,$10,$11,1,$12,0.98,'structured')`,
			e.id, bookID, e.docID, e.entityType, e.subtype, e.amountCents, dc, e.txnDate,
			e.counterparty, e.description, glCode,
			`{"x":0.1,"y":0.1,"width":0.8,"height":0.05}`)
		if err != nil {
			fmt.Fprintf(os.Stderr, "seed-demo: extracted_entity: %v\n", err)
			os.Exit(1)
		}
	}

	for _, g := range groups {
		_, err := pool.Exec(ctx, `
			INSERT INTO reconciliation_groups (id, client_book_id, link_confidence, status)
			VALUES ($1, $2, $3, $4)`,
			g.id, bookID, g.confidence, g.status)
		if err != nil {
			fmt.Fprintf(os.Stderr, "seed-demo: group: %v\n", err)
			os.Exit(1)
		}
		members := []struct {
			role string
			eid  uuid.UUID
		}{
			{"invoice", g.invID}, {"bank", g.bankID}, {"gl", g.glID},
		}
		for _, m := range members {
			_, err := pool.Exec(ctx, `
				INSERT INTO reconciliation_group_members (reconciliation_group_id, extracted_entity_id, role)
				VALUES ($1, $2, $3)`,
				g.id, m.eid, m.role)
			if err != nil {
				fmt.Fprintf(os.Stderr, "seed-demo: group member: %v\n", err)
				os.Exit(1)
			}
		}

		// Finding per group, tracing the planted variance back to the source doc.
		d := g.discrepancy
		variance := max3(d.InvoiceCents, d.BankCents, d.GLCents) - min3(d.InvoiceCents, d.BankCents, d.GLCents)
		exceeds := variance > tolerance
		sev := d.Severity()
		formula := fmt.Sprintf("max(|inv-bank|,|inv-gl|,|bank-gl|) = %d cents vs tolerance %d", variance, tolerance)
		_, err = pool.Exec(ctx, `
			INSERT INTO audit_findings
				(id, client_book_id, reconciliation_group_id, rule_id, rule_version,
				 calculated_variance_cents, tolerance_cents, exceeds_tolerance,
				 calculation_formula, severity, status, prepared_by)
			VALUES ($1,$2,$3,'three_way_reconciliation','1.0',$4,$5,$6,$7,$8,'open',$9)`,
			uuid.New(), bookID, g.id, variance, tolerance, exceeds, formula, sev, userID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "seed-demo: finding: %v\n", err)
			os.Exit(1)
		}
	}

	return bookSummary{
		name: bookName, entityCount: len(entities),
		bySeverity: bySeverity, exactMatches: exact, withinTol: withinTol, mismatches: mismatches,
	}
}

func max3(a, b, c int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
