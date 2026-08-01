package humanoverride

import "testing"

// TestStrVal covers the value-to-string coercion used by the config audit log.
func TestStrVal(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},          // nil -> nil pointer handled separately; strVal(nil) == nil
		{"0.85", "0.85"},   // string passthrough
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
