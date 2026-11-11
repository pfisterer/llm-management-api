package usage

import (
	"context"
	"testing"
	"time"
)

// The day a sample is booked to is its day in Berlin: around midnight in Berlin and around the changes of summer time.
func TestDayIsBerlin(t *testing.T) {
	for _, tc := range []struct{ at, want string }{
		{"2026-11-11T22:59:59Z", "2026-11-11"}, // 23:59:59 CET
		{"2026-11-11T23:00:00Z", "2026-11-12"}, // midnight CET, still the 11th in UTC
		{"2026-11-11T23:30:00Z", "2026-11-12"},
		{"2026-07-01T21:59:59Z", "2026-07-01"}, // 23:59:59 CEST
		{"2026-07-01T22:00:00Z", "2026-07-02"}, // midnight CEST, two hours before UTC's
		{"2026-10-25T00:30:00Z", "2026-10-25"}, // 02:30 CEST, the first of the two 02:30 of the autumn change
		{"2026-10-25T01:30:00Z", "2026-10-25"}, // 02:30 CET, the second one
		{"2026-10-25T22:59:59Z", "2026-10-25"}, // the 25-hour day ends at 23:00 UTC
		{"2026-10-25T23:00:00Z", "2026-10-26"},
		{"2027-03-27T23:00:00Z", "2027-03-28"}, // midnight CET before the spring change
		{"2027-03-28T21:59:59Z", "2027-03-28"}, // the 23-hour day ends at 22:00 UTC
		{"2027-03-28T22:00:00Z", "2027-03-29"},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		got := Day(at)
		if got.Format(time.DateOnly) != tc.want || got.Location() != time.UTC || got.Hour() != 0 {
			t.Errorf("Day(%s) = %v, want %s at midnight UTC", tc.at, got, tc.want)
		}
	}
}

// A minute's sample just after midnight in Berlin goes to the new day's row, not to the one before.
func TestCollectorBooksBerlinDay(t *testing.T) {
	st := NewMemoryStore()
	c := NewCollector(st, func(context.Context) ([]Sample, error) { return []Sample{{Project: "p", Person: "a@dhbw.de", Profile: "cpu", CPU: 2}}, nil }, nil, time.Minute, nil)
	c.now = func() time.Time { return time.Date(2026, 11, 11, 23, 0, 30, 0, time.UTC) } // 00:00:30 on 12.11. in Berlin
	if err := c.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Range(t.Context(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	if err != nil || len(rows) != 1 || rows[0].Day.Format(time.DateOnly) != "2026-11-12" {
		t.Fatalf("rows %+v, %v; want one row of 2026-11-12", rows, err)
	}
}
