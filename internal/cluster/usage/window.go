package usage

import (
	"time"

	"github.com/pfisterer/cloud-self-service-golib/calendar"
)

// The rules for a span of days come from golib calendar, the contract with the budget tree (openstack-management-api): a window of a limit counts today and the days before it, so what is used today counts at once (calendar.Window.Start); a report counts finished days only, so its numbers do not change while one reads them (calendar.Report). Both count in calendar days in Europe/Berlin, as usage is booked (Day).

// MaxReportDays is the longest period a report of the daily rows covers (three years, like the "lifetime" view of the cloud projects' report).
const MaxReportDays = 3 * 366

// Reports is the period rule of the reports of daily rows: by default the last 30 finished days, at most MaxReportDays.
var Reports = calendar.Report(MaxReportDays)

// GPUHourReports is the period rule of the hourly GPU report: its numbers are live, so by default the last 7 days including today, at most as far back as the rows are kept.
var GPUHourReports = calendar.Rule{Days: 7, IncludeToday: true, MaxDays: int(GPUHourRetention / (24 * time.Hour))}

// LLMWindow is the window of a project's LLM tokens, the budget duration of its LiteLLM team, which resets after it (a rolling reset in LiteLLM, not calendar days).
const LLMWindow = calendar.Window30d
