package usage

import (
	"testing"
	"time"
)

func TestWindows(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	for days, want := range map[int]string{1: "2026-11-10", 7: "2026-11-04", 30: "2026-10-12"} {
		if got := LimitWindowStart(now, days); !got.Equal(day(want)) {
			t.Errorf("limit %d days starts %s, want %s", days, got, want)
		}
	}
	from, to := ReportPeriod(now, 30)
	if !from.Equal(day("2026-10-11")) || !to.Equal(day("2026-11-09")) {
		t.Errorf("report 30 days: %s to %s", from, to)
	}
}
