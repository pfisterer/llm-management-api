package workspace

import (
	"context"
	"fmt"
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
	Used      float64 `json:"used"`
	Exhausted bool    `json:"exhausted"`
}

// windowStart is the first day a window counts. Usage is booked per UTC day, so the windows are calendar days: 24h = today, 7d and 30d including today, total = since the project was created.
func windowStart(window string, now time.Time, created time.Time) time.Time {
	today := usage.Day(now)
	switch window {
	case "24h":
		return today
	case "7d":
		return today.AddDate(0, 0, -6)
	case "30d":
		return today.AddDate(0, 0, -29)
	default:
		if created.IsZero() {
			return today.AddDate(-3, 0, 0)
		}
		return usage.Day(created)
	}
}

func amountOf(r usage.Row, b Budget) float64 {
	switch b.Unit {
	case "server_hours":
		return r.ServerHours
	case "vcpu_hours":
		return r.VCPUHours
	case "ram_gb_hours":
		return r.RAMGBHours
	case "gpu_hours":
		if b.GPUClass != "" && r.GPUClass != b.GPUClass {
			return 0
		}
		return r.GPUHours
	}
	return 0
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
				return refuseStart("budget", "Budget im Projekt „%s“ aufgebraucht: %s. Genutzt: %s.", p.Name, describe(b.Budget), num(b.Used))
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
	earliest := now
	for _, b := range p.Budgets {
		if w := windowStart(b.Window, now, p.CreatedAt); w.Before(earliest) {
			earliest = w
		}
	}
	rows, err := s.usage.Range(ctx, earliest, usage.Day(now))
	if err != nil {
		return nil, err
	}
	for _, b := range p.Budgets {
		from := windowStart(b.Window, now, p.CreatedAt)
		st := BudgetStatus{Budget: b}
		for _, r := range rows {
			if r.Project != p.Slug || r.Day.Before(from) || (b.PerPerson && !strings.EqualFold(r.Person, person)) {
				continue
			}
			st.Used += amountOf(r, b)
		}
		st.Exhausted = b.Amount > 0 && st.Used >= b.Amount
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
		if r.Project == "" || r.Profile == "gpu-kernel" {
			continue // kernels end with their server
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
						reason = fmt.Sprintf("%s (genutzt %s)", describe(b.Budget), num(b.Used))
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

var unitNames = map[string]string{"server_hours": "Workspace-Stunden", "vcpu_hours": "vCPU-Stunden", "ram_gb_hours": "RAM-GB-Stunden", "gpu_hours": "GPU-Stunden"}
var windowNames = map[string]string{"24h": "heute", "7d": "in 7 Tagen", "30d": "in 30 Tagen", "total": "insgesamt"}

func describe(b Budget) string {
	unit := unitNames[b.Unit]
	if b.Unit == "gpu_hours" && b.GPUClass != "" {
		unit += " (" + b.GPUClass + ")"
	}
	who := "für das ganze Projekt"
	if b.PerPerson {
		who = "pro Person"
	}
	return fmt.Sprintf("%s %s %s %s", num(b.Amount), unit, windowNames[b.Window], who)
}

func num(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".")
}
