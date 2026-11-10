package usage

import "time"

// The two rules for a span of days (decided 10.11.2026, docs/refactoring-audit.md), in one place: a limit counts today and the days before it, so what is used today counts at once; a report counts finished days only, so its numbers do not change while one reads them. Both count in UTC days, as usage is booked.

// LimitWindowStart is the first day a limit over days counts: today and the days-1 before it (24h = 1: today; 7d = 7; 30d = 30).
func LimitWindowStart(now time.Time, days int) time.Time {
	return Day(now).AddDate(0, 0, -(days - 1))
}

// ReportPeriod is a report's default span of days: the last days finished days, from the day days ago to yesterday, both included.
func ReportPeriod(now time.Time, days int) (from, to time.Time) {
	today := Day(now)
	return today.AddDate(0, 0, -days), today.AddDate(0, 0, -1)
}

// LLMWindow is the window of a project's LLM tokens, the budget duration of its LiteLLM team, which resets after it (a rolling reset in LiteLLM, not calendar days).
const LLMWindow = "30d"
