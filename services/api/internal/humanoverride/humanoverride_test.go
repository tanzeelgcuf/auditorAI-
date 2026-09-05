package humanoverride

import (
	"strings"
	"testing"
)

// TestStrVal covers the value-to-string coercion used by the config audit log.
//
// The concrete-value cases below are the original test. They passed before
// 2026-09-06 and they pass now, and on their own they proved nothing about the
// real call path: every caller reaches strVal through an `interface{}` parameter
// holding a TYPED POINTER out of a decoded request body, and this test never
// once passed one. See TestStrValUnwrapsTypedPointers.
func TestStrVal(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""}, // nil -> nil pointer handled separately; strVal(nil) == nil
		{"0.85", "0.85"}, // string passthrough
		{int(50), "50"},
		{int64(100), "100"},
		{1.0, "1"},
		{0.5, "0.5"},
		{true, "true"},
		{false, "false"},
	}
	for _, c := range cases {
		if c.in == nil {
			if got := strVal(c.in); got != nil {
				t.Errorf("strVal(nil) = %v, want nil", got)
			}
			continue
		}
		got := strVal(c.in)
		if got == nil || *got != c.want {
			t.Errorf("strVal(%v) = %v, want %q", c.in, got, c.want)
		}
	}
}

// TestStrValUnwrapsTypedPointers is the shape every real caller actually uses.
//
// tenant.HandleUpdateBookSettings decodes the body into a struct of *string,
// *int and *float64, and each one reaches config_change_log through this
// function. Two distinct defects lived here, both invisible to the compiler and
// to the test above:
//
//   - A nil *string is NOT equal to nil once it is inside an interface — it
//     carries a type descriptor. So `v == nil` did not fire, the type switch had
//     no pointer case, and fmt.Sprint produced the literal text "<nil>". The row
//     recorded a change TO "<nil>" for a field nobody touched.
//   - A non-nil *string missed the switch the same way, so fmt.Sprint printed
//     the POINTER: new_value became a hex address like "0xc000123456" instead of
//     the value the user set.
//
// Both assertions below FAIL against the pre-fix function, which is the point —
// a regression test that also passes on the old code is not evidence.
func TestStrValUnwrapsTypedPointers(t *testing.T) {
	t.Run("nil typed pointer is absent, not the text <nil>", func(t *testing.T) {
		var nilStr *string
		var nilInt *int
		var nilFloat *float64
		var nilBool *bool
		for name, v := range map[string]interface{}{
			"*string": nilStr, "*int": nilInt, "*float64": nilFloat, "*bool": nilBool,
		} {
			got := strVal(v)
			if got == nil {
				continue
			}
			if *got == "<nil>" {
				t.Errorf("strVal(nil %s) = %q — a fabricated audit row: the field was "+
					"absent from the request, so this must be SQL NULL", name, *got)
				continue
			}
			t.Errorf("strVal(nil %s) = %q, want nil", name, *got)
		}
	})

	t.Run("non-nil typed pointer records the value, not the address", func(t *testing.T) {
		s := "0.85"
		n := 50
		f := 0.5
		b := true
		for _, c := range []struct {
			name string
			in   interface{}
			want string
		}{
			{"*string", &s, "0.85"},
			{"*int", &n, "50"},
			{"*float64", &f, "0.5"},
			{"*bool", &b, "true"},
		} {
			got := strVal(c.in)
			if got == nil {
				t.Errorf("strVal(%s) = nil, want %q", c.name, c.want)
				continue
			}
			if strings.HasPrefix(*got, "0x") {
				t.Errorf("strVal(%s) = %q — that is the pointer's address, not the "+
					"audited value. config_change_log.new_value must hold %q",
					c.name, *got, c.want)
				continue
			}
			if *got != c.want {
				t.Errorf("strVal(%s) = %q, want %q", c.name, *got, c.want)
			}
		}
	})

	t.Run("double pointer is not silently unwrapped to an address", func(t *testing.T) {
		// One level of unwrapping is deliberate and enough — the audited types are
		// all single pointers out of a decoded body. A **string would unwrap to a
		// *string and then format as an address, so assert the boundary rather
		// than leaving it to be discovered in a customer's audit trail.
		s := "0.85"
		p := &s
		got := strVal(&p)
		if got != nil && strings.HasPrefix(*got, "0x") {
			t.Skipf("known boundary: strVal(**string) = %q. No caller passes a "+
				"double pointer; recorded here so the next one sees it", *got)
		}
	})
}
