package usage

import (
	"context"
	"sort"
	"time"
)

// Total is the sum of rows: one entry per project, person, profile and GPU class over the period, or one per day.
type Total struct {
	Project  string `json:"project,omitempty"`
	Person   string `json:"person,omitempty"`
	Profile  string `json:"profile,omitempty"`
	GPUClass string `json:"gpu_class,omitempty"`
	// GPUProduct is the GPU type used (nvidia.com/gpu.product of the node).
	GPUProduct string    `json:"gpu_product,omitempty"`
	Day        time.Time `json:"day,omitzero"`
	Days       int       `json:"days"` // days with usage

	ServerHours    float64   `json:"server_hours"`
	VCPUHours      float64   `json:"vcpu_hours"`
	RAMGBHours     float64   `json:"ram_gb_hours"`
	GPUHours       float64   `json:"gpu_hours"`
	CPUUsedHours   float64   `json:"cpu_used_hours"`
	RAMUsedGBHours float64   `json:"ram_used_gb_hours"`
	HomeGBDays     float64   `json:"home_gb_days"` // sum over days of the day's home size
	LastActive     time.Time `json:"last_active,omitzero"`
}

func (t *Total) add(r Row) {
	t.Days++
	t.ServerHours += r.ServerHours
	t.VCPUHours += r.VCPUHours
	t.RAMGBHours += r.RAMGBHours
	t.GPUHours += r.GPUHours
	t.CPUUsedHours += r.CPUUsedHours
	t.RAMUsedGBHours += r.RAMUsedGBHours
	t.HomeGBDays += r.HomeGB
	if r.ServerHours > 0 && r.Day.After(t.LastActive) {
		t.LastActive = r.Day
	}
}

// Report is the usage of a period, as the self-service UI shows it. Grouping (by project, person, profile, GPU class) is the UI's job, from Entries.
type Report struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Total   Total     `json:"total"`
	PerDay  []Total   `json:"per_day"`
	Entries []Total   `json:"entries"`
}

// Filter limits a report, e.g. to one person (own usage) or the projects someone manages. nil = everything.
type Filter func(Row) bool

// BuildReport sums the stored rows of [from, to] (both days included).
func BuildReport(ctx context.Context, st Store, from, to time.Time, keep Filter) (Report, error) {
	from, to = Day(from), Day(to)
	rows, err := st.Range(ctx, from, to)
	if err != nil {
		return Report{}, err
	}
	rep := Report{From: from, To: to, PerDay: []Total{}, Entries: []Total{}}
	type ek struct{ project, person, profile, gpu, product string }
	entries := map[ek]*Total{}
	days := map[time.Time]*Total{}
	for _, r := range rows {
		if keep != nil && !keep(r) {
			continue
		}
		rep.Total.add(r)
		k := ek{r.Project, r.Person, r.Profile, r.GPUClass, r.GPUProduct}
		if entries[k] == nil {
			entries[k] = &Total{Project: r.Project, Person: r.Person, Profile: r.Profile, GPUClass: r.GPUClass, GPUProduct: r.GPUProduct}
		}
		entries[k].add(r)
		if days[r.Day] == nil {
			days[r.Day] = &Total{Day: r.Day}
		}
		days[r.Day].add(r)
	}
	rep.Total.Days = len(days)
	for _, t := range entries {
		rep.Entries = append(rep.Entries, *t)
	}
	sort.Slice(rep.Entries, func(i, j int) bool {
		a, b := rep.Entries[i], rep.Entries[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.GPUClass != b.GPUClass {
			return a.GPUClass < b.GPUClass
		}
		return a.GPUProduct < b.GPUProduct
	})
	for _, t := range days {
		t.Days = 1
		rep.PerDay = append(rep.PerDay, *t)
	}
	sort.Slice(rep.PerDay, func(i, j int) bool { return rep.PerDay[i].Day.Before(rep.PerDay[j].Day) })
	return rep, nil
}
