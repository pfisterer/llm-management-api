package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/usage"
	"go.uber.org/zap"
)

// UsageReader is where the booked usage comes from (usage.Store).
type UsageReader interface {
	Range(ctx context.Context, from, to time.Time) ([]usage.Row, error)
}

// BudgetStatus is one budget of a project with what is used of it: by the person for a budget per person, by the whole project otherwise.
type BudgetStatus struct {
	Budget
	Used float64 `json:"used"`
	// Reserved is what running and waiting batch jobs still hold of a GPU-hours budget (their GPUs × the hours they may still run); it counts as used for Exhausted.
	Reserved  float64 `json:"reserved"`
	Exhausted bool    `json:"exhausted"`
}

// unitDef is a unit of budgets: its name for people and what a usage row adds to it.
type unitDef struct {
	label  string
	amount func(usage.Row) float64
}

// units are the units of workspace budgets (Budget.Unit); a unit not listed counts nothing.
var units = map[string]unitDef{
	"server_hours": {"Workspace-Stunden", func(r usage.Row) float64 { return r.ServerHours }},
	"vcpu_hours":   {"vCPU-Stunden", func(r usage.Row) float64 { return r.VCPUHours }},
	"ram_gb_hours": {"RAM-GB-Stunden", func(r usage.Row) float64 { return r.RAMGBHours }},
	"gpu_hours":    {"GPU-Stunden", func(r usage.Row) float64 { return r.GPUHours }},
}

// windowDef is a window of budgets: its name for people, its length in days (0 = since the project was created) and whether the budget tree gives amounts per it.
type windowDef struct {
	label string
	days  int
	tree  bool
}

// windows are the windows of workspace budgets (Budget.Window). The tree gives amounts per 24h, 7d or 30d only, never one for a project's whole term (decided 10.11.2026); a window not listed counts like total.
var windows = map[string]windowDef{
	"24h":   {"heute", 1, true},
	"7d":    {"in 7 Tagen", 7, true},
	"30d":   {"in 30 Tagen", 30, true},
	"total": {"insgesamt", 0, false},
}

// windowStart is the first day a window counts. Usage is booked per UTC day, so the windows are calendar days: 24h = today, 7d and 30d including today (usage.LimitWindowStart), total = since the project was created.
func windowStart(window string, now time.Time, created time.Time) time.Time {
	if w := windows[window]; w.days > 0 {
		return usage.LimitWindowStart(now, w.days)
	}
	if created.IsZero() {
		return usage.Day(now).AddDate(-3, 0, 0)
	}
	return usage.Day(created)
}

// earliestStart is the first day any of the budgets counts, at the latest today: from there the usage rows are read once for all of them.
func earliestStart(budgets []Budget, now, created time.Time) time.Time {
	earliest := usage.Day(now)
	for _, b := range budgets {
		if w := windowStart(b.Window, now, created); w.Before(earliest) {
			earliest = w
		}
	}
	return earliest
}

// coversClass tells whether the budget counts GPUs of class: one of GPU hours counts its class only, when it names one.
func (b Budget) coversClass(class string) bool {
	return b.Unit != "gpu_hours" || b.GPUClass == "" || b.GPUClass == class
}

// usedIn is what the rows of project from the day from on add to b; of person only, for a budget per person. BudgetStatus (starts, jobs, Enforce) and the metrics both count with it.
func usedIn(rows []usage.Row, project string, from time.Time, b Budget, person string) float64 {
	amount := units[b.Unit].amount
	var used float64
	for _, r := range rows {
		if r.Project != project || r.Day.Before(from) || (b.PerPerson && !strings.EqualFold(r.Person, person)) || amount == nil || !b.coversClass(r.GPUClass) {
			continue
		}
		used += amount(r)
	}
	return used
}

// SetUsage turns on budgets: CheckStart refuses a start whose budget is used up, BudgetStatus reports them.
func (s *Service) SetUsage(u UsageReader) {
	s.usage = u
	s.budget = func(ctx context.Context, p Project, person, _ string) error {
		st, err := s.BudgetStatus(ctx, p, person)
		if err != nil {
			return err
		}
		for _, b := range st {
			if b.Exhausted {
				return refuseStart("budget", "Budget im Projekt „%s“ aufgebraucht: %s. Genutzt: %s%s.", p.Name, describe(b.Budget), num(b.Used), reservedNote(b))
			}
		}
		return nil
	}
}

// BudgetStatus reports every budget of the project for the person.
func (s *Service) BudgetStatus(ctx context.Context, p Project, person string) ([]BudgetStatus, error) {
	out := []BudgetStatus{}
	if s.usage == nil || len(p.Budgets) == 0 {
		return out, nil
	}
	now := s.now()
	rows, err := s.usage.Range(ctx, earliestStart(p.Budgets, now, p.CreatedAt), usage.Day(now))
	if err != nil {
		return nil, err
	}
	var rs []Reservation
	if s.reservations != nil && slices.ContainsFunc(p.Budgets, func(b Budget) bool { return b.Unit == "gpu_hours" }) {
		if rs, err = s.reservations(ctx, p.Slug); err != nil {
			return nil, err
		}
	}
	for _, b := range p.Budgets {
		st := BudgetStatus{Budget: b, Used: usedIn(rows, p.Slug, windowStart(b.Window, now, p.CreatedAt), b, person)}
		st.Reserved = reserved(rs, person, b)
		st.Exhausted = b.Amount > 0 && st.Used+st.Reserved >= b.Amount
		out = append(out, st)
	}
	return out, nil
}

// Stopper ends a person's server (JupyterHub: server name "" = the default server).
type Stopper func(ctx context.Context, person, server string) error

// RunningServer is a running server for Enforce.
type RunningServer struct {
	Running
	ServerName string // JupyterHub's name of the server
}

// Enforce ends the servers of people whose budget in the server's project is used up. The home stays; starting again is refused until the window frees budget.
func (s *Service) Enforce(ctx context.Context, running []RunningServer, stop Stopper, log *zap.SugaredLogger) {
	if s.usage == nil {
		return
	}
	type key struct{ project, person string }
	decided := map[key]string{} // reason or ""
	for _, r := range running {
		if r.Project == "" || !IsServerProfile(r.Profile) {
			continue // kernels end with their server, jobs run to their end (their budget is reserved at submit)
		}
		k := key{r.Project, strings.ToLower(r.Person)}
		reason, done := decided[k]
		if !done {
			p, err := s.store.Get(ctx, r.Project)
			if err == nil {
				st, err := s.BudgetStatus(ctx, p, r.Person)
				if err != nil {
					log.Warnw("budget status", "project", r.Project, "error", err)
				}
				for _, b := range st {
					if b.Exhausted {
						reason = fmt.Sprintf("%s (genutzt %s%s)", describe(b.Budget), num(b.Used), reservedNote(b))
						break
					}
				}
			}
			decided[k] = reason
		}
		if reason == "" {
			continue
		}
		if err := stop(ctx, r.Person, r.ServerName); err != nil {
			log.Warnw("budget: stopping a server failed", "person", r.Person, "server", r.ServerName, "project", r.Project, "error", err)
			continue
		}
		log.Infow("budget used up, server stopped", "person", r.Person, "server", r.ServerName, "project", r.Project, "budget", reason)
	}
}

// reservedNote names what jobs reserve of a budget, for the reasons; empty when nothing is reserved.
func reservedNote(b BudgetStatus) string {
	if b.Reserved <= 0 {
		return ""
	}
	return ", für Jobs vorgemerkt " + num(b.Reserved)
}

func describe(b Budget) string {
	unit := units[b.Unit].label
	if b.Unit == "gpu_hours" && b.GPUClass != "" {
		unit += " (" + b.GPUClass + ")"
	}
	who := "für das ganze Projekt"
	if b.PerPerson {
		who = "pro Person"
	}
	return fmt.Sprintf("%s %s %s %s", num(b.Amount), unit, windows[b.Window].label, who)
}

// num writes a number for people: one decimal, two below 1 (job reservations are often fractions of an hour).
func num(v float64) string {
	f := "%.1f"
	if v != 0 && v < 1 && v > -1 {
		f = "%.2f"
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf(f, v), "0"), ".")
}
