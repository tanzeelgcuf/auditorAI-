package connectors

// decimal_test.go — the connector's decimal→cents conversion, pure.
//
// The conversion is a string parse, NO float, the same semantics as the Rust
// money parser: commas, parenthesised negatives, a leading minus, and
// anything ambiguous (>2 decimal places, an empty string) FAILS rather than
// being rounded or defaulted to zero — the parser's own rule. This is the
// third money-parse implementation in the repo (Rust source, Python mirror,
// this Go one) — the parity-guard class; it should join a parity check when
// it earns one.

import (
	"testing"
)

func TestDecimalToCents(t *testing.T) {
	// The expectations mirror the Rust parser's normalize_decimal
	// (structured.rs:152-236) — the parity is the point. The 1-digit-fraction
	// padding ("1500.5" -> 150050, not 150005) was caught by the port parity
	// check, not by reasoning; the Rust parser's own comment documents it.
	cases := []struct {
		in   string
		want int64
	}{
		{"1500.00", 150000},
		{"1500.5", 150050}, // the 1-digit fraction pads to tenths
		{"1,234.56", 123456},
		{"1.234,56", 123456}, // the European form: the rightmost is the decimal
		{"-45.00", -4500},
		{"(45.00)", -4500},
		{"0.00", 0},
		{"0", 0},
		{".50", 50}, // an empty integer part with a decimal is legitimate
		{"199999.00", 19999900},
		{"-1946646", -194664600},
		{"1.23", 123},
	}
	for _, tc := range cases {
		got, err := decimalToCents(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDecimalToCents_FailsOnAmbiguous(t *testing.T) {
	// The parser's rule: an amount it cannot read unambiguously FAILS — no
	// rounding, no zero default. "1,234,567" is rejected EVEN THOUGH its
	// grouping is valid: a repeated separator with no decimal evidence means
	// the convention is not decidable there, and the Rust parser rejects.
	// "1,23,456.78" has an invalid grouping (the second group is 2 digits).
	for _, in := range []string{"", "abc", "1.2.3", "1.234", "  ", "1,2,3", "1,234,567", "1,23,456.78"} {
		if got, err := decimalToCents(in); err == nil {
			t.Errorf("%q: parsed to %d, want a failure (ambiguous amounts must not silently parse)", in, got)
		}
	}
}
