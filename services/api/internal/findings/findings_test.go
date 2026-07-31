package findings

import (
	"strings"
	"testing"
)

func TestRuleVersionHashConsistent(t *testing.T) {
	content := `{"rules": [{"variance_cents": "[0..1]", "severity": "info"}]}`
	v1 := ruleVersionHash(content)
	v2 := ruleVersionHash(content)
	if v1 != v2 {
		t.Fatalf("same rule content must produce same version: %s != %s", v1, v2)
	}
	if len(v1) != 16 {
		t.Fatalf("expected 16-hex-char version, got %d chars", len(v1))
	}
}

func TestRuleVersionHashChangesWithContent(t *testing.T) {
	a := ruleVersionHash(`{"rules":[{"severity":"info"}]}`)
	b := ruleVersionHash(`{"rules":[{"severity":"high"}]}`)
	if a == b {
		t.Fatalf("different rule content must produce different versions")
	}
}

func TestBuildTypstSource(t *testing.T) {
	d := &reportData{findings: []findingEntry{
		{ID: "f1", RuleID: "gl_reconciliation", RuleVersion: "abc123", VarianceCents: 50,
			ToleranceCents: 1, Exceeds: true, Formula: "sum(1200.00, 340.50)=1540.50", Severity: "medium", Status: "open"},
		{ID: "f2", RuleID: "gl_reconciliation", RuleVersion: "abc123", VarianceCents: 0,
			ToleranceCents: 1, Exceeds: false, Formula: "exact", Severity: "info", Status: "resolved"},
	}}

	src := buildTypstSource(d)
	for _, want := range []string{"= Audit Report", "gl_reconciliation vabc123", "severity: medium", "Methodology", "AI-assisted"} {
		if !strings.Contains(src, want) {
			t.Errorf("Typst source missing %q", want)
		}
	}
	if !strings.Contains(src, "high: 0") {
		t.Errorf("Typst source should tally high severity count")
	}
}

func TestBuildTypstSourceEmptyFindings(t *testing.T) {
	src := buildTypstSource(&reportData{})
	if !strings.Contains(src, "Findings by severity:") {
		t.Errorf("empty report should still include severity tally header")
	}
}
