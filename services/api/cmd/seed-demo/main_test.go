package main

import (
	"math/rand"
	"testing"
)

// TestDiscrepancySeverity checks the severity bands match the verification
// decision graph (services/verification/decision-graphs/gl_reconciliation.json):
//   info   = variance <= tolerance
//   low    = tolerance < variance <= 10*tolerance
//   medium = 10*tolerance < variance <= 100*tolerance
//   high   = 100*tolerance < variance
func TestDiscrepancySeverity(t *testing.T) {
	cases := []struct {
		name string
		d    AmountDiscrepancy
		want string
	}{
		{"exact", AmountDiscrepancy{InvoiceCents: 10000, BankCents: 10000, GLCents: 10000, ToleranceCents: 100}, "info"},
		{"within tolerance", AmountDiscrepancy{InvoiceCents: 10000, BankCents: 10015, GLCents: 10000, ToleranceCents: 100}, "info"},
		{"over tolerance -> low", AmountDiscrepancy{InvoiceCents: 10000, BankCents: 10000, GLCents: 10150, ToleranceCents: 100}, "low"},
		{"medium band", AmountDiscrepancy{InvoiceCents: 10000, BankCents: 10000, GLCents: 15000, ToleranceCents: 100}, "medium"},
		{"high band", AmountDiscrepancy{InvoiceCents: 10000, BankCents: 10000, GLCents: 25000, ToleranceCents: 100}, "high"},
	}
	for _, tc := range cases {
		if got := tc.d.Severity(); got != tc.want {
			t.Errorf("%s: Severity() = %q, want %q (discrepancy %+v)", tc.name, got, tc.want, tc.d)
		}
	}
}

// TestDiscrepancyPlanMix verifies the planted plan is deterministic for a seed
// and yields exactly 3 exact matches, 2 within-tolerance variances (info), and
// 2 genuine high-severity mismatches. Offline: no DB, no network.
func TestDiscrepancyPlanMix(t *testing.T) {
	plan := discrepancyPlan(20240101, 100)
	if len(plan) != 7 {
		t.Fatalf("plan has %d discrepancies, want 7", len(plan))
	}
	bySev := map[string]int{}
	for _, d := range plan {
		bySev[d.Severity()]++
	}
	if bySev["info"] != 5 {
		t.Errorf("info count = %d, want 5 (3 exact + 2 within-tolerance)", bySev["info"])
	}
	if bySev["high"] != 2 {
		t.Errorf("high count = %d, want 2", bySev["high"])
	}
	for sev, n := range bySev {
		if sev != "info" && sev != "high" {
			t.Errorf("unexpected severity %q (n=%d) in planted plan", sev, n)
		}
	}
	// Determinism: same seed -> same plan.
	plan2 := discrepancyPlan(20240101, 100)
	if len(plan2) != len(plan) {
		t.Fatalf("re-seeded plan differs in length")
	}
	for i := range plan {
		if plan[i] != plan2[i] {
			t.Fatalf("re-seeded plan differs at index %d: %+v vs %+v", i, plan[i], plan2[i])
		}
	}
}

// discrepancyPlan is the plan generator used by seedBook; extracted from main.go
// so the pure logic is testable without a DB. It is a deliberate duplicate of
// the loop in seedBook — keep in sync (ponytail: promote to a shared helper if
// plan parameters ever become CLI-configurable).
func discrepancyPlan(seed int64, tolerance int) []AmountDiscrepancy {
	rng := rand.New(rand.NewSource(seed))
	var plan []AmountDiscrepancy
	for i := 0; i < 3; i++ {
		base := 10000 + rng.Intn(90000) // $100-$1000
		disc := rng.Intn(30) - 15       // ±$0.15 → exact (within $1 tolerance)
		plan = append(plan, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base + disc,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	for i := 0; i < 2; i++ {
		base := 20000 + rng.Intn(80000)
		disc := 1 + rng.Intn(tolerance) // within tolerance → info finding
		plan = append(plan, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	for i := 0; i < 2; i++ {
		base := 50000 + rng.Intn(200000)
		disc := tolerance*100 + rng.Intn(50000) // > 100x tolerance → high severity
		plan = append(plan, AmountDiscrepancy{
			InvoiceCents:   base,
			BankCents:      base,
			GLCents:        base + disc,
			ToleranceCents: tolerance,
		})
	}
	rng.Shuffle(len(plan), func(i, j int) {
		plan[i], plan[j] = plan[j], plan[i]
	})
	return plan
}
