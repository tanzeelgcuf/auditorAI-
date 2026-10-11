package auditlog

// auditlog_test.go — the retention-window guard (pure, no DB).
//
// The retention window is the SOC2 "Audit-log IP retention policy" control's
// only knob. A bad env value must degrade to the default, never crash the
// sweeper at boot and never delete everything (a zero or negative window
// interpreted literally would delete every row every sweep).

import (
	"testing"
)

func TestRetentionDays(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 90},
		{"30", 30},
		{"bogus", 90},
		{"-5", 90},
		{"0", 90},
	}
	for _, tc := range cases {
		t.Setenv("AUDIT_LOG_RETENTION_DAYS", tc.env)
		if got := RetentionDays(); got != tc.want {
			t.Errorf("AUDIT_LOG_RETENTION_DAYS=%q: got %d, want %d", tc.env, got, tc.want)
		}
	}
}
