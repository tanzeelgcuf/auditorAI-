package periods

import (
	"testing"
	"time"
)

// --- pure due-date logic (mirrors the SQL CASE in HandleFirmDashboard) ---

func dueWindow(sev string) int {
	switch sev {
	case "high":
		return 2
	case "medium":
		return 7
	case "low":
		return 30
	default:
		return 0
	}
}

// dueDate approximates the SQL expression
//   created_at::date + days + 2*(days/7)
// which adds the severity window plus weekend padding for the business-day
// interpretation. Matches the dashboard's past-due predicate.
func dueDate(created time.Time, sev string) time.Time {
	d := dueWindow(sev)
	return created.AddDate(0, 0, d+2*(d/7))
}

func TestDueDateLogic(t *testing.T) {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	cases := []struct {
		sev  string
		want time.Time
	}{
		{"high", base.AddDate(0, 0, 2)},  // 2bd -> Wed Jan 7
		{"medium", base.AddDate(0, 0, 9)}, // 7bd -> +7 days + 2 weekend days
		{"low", base.AddDate(0, 0, 38)},   // 30bd -> +30 + 8
		{"info", base},                    // no due window
		{"bogus", base},                   // unknown severity treated as none
	}
	for _, c := range cases {
		if got := dueDate(base, c.sev); !got.Equal(c.want) {
			t.Errorf("dueDate(%s) = %s, want %s", c.sev, got.Format("2006-01-02"), c.want.Format("2006-01-02"))
		}
	}
}

func TestDueDatePastDue(t *testing.T) {
	// A high-severity finding is past due once today > created + 2bd.
	created := time.Now().AddDate(0, 0, -10)
	due := dueDate(created, "high")
	if !due.Before(time.Now()) {
		t.Errorf("high finding from 10 days ago should be past due, due=%s", due.Format("2006-01-02"))
	}
	// A fresh finding is not past due.
	fresh := dueDate(time.Now(), "low")
	if !fresh.After(time.Now()) {
		t.Errorf("fresh low finding should not be past due")
	}
}

// --- pure overlap rejection (mirrors the SQL && range predicate) ---

type dateRange struct{ start, end time.Time }

func overlaps(a, b dateRange) bool {
	return !a.end.Before(b.start) && !b.end.Before(a.start)
}

func TestPeriodOverlap(t *testing.T) {
	jan := dateRange{start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), end: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)}

	cases := []struct {
		name string
		o    dateRange
		want bool
	}{
		{"inside", dateRange{time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)}, true},
		{"shares-start", dateRange{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)}, true},
		{"shares-end", dateRange{time.Date(2025, 12, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)}, true},
		{"adjacent", dateRange{time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)}, false},
		{"month-apart", dateRange{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)}, false},
	}
	for _, c := range cases {
		if got := overlaps(jan, c.o); got != c.want {
			t.Errorf("%s: overlaps = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	valid, ok := parseDate("2026-01-31")
	if !ok || valid.Year() != 2026 || valid.Month() != time.January || valid.Day() != 31 {
		t.Errorf("valid date failed: %v %v", valid, ok)
	}
	if _, ok := parseDate("31-01-2026"); ok {
		t.Errorf("wrong layout should fail")
	}
	if _, ok := parseDate("2026-02-31"); ok {
		t.Errorf("impossible date should fail")
	}
}
