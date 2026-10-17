package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
	"github.com/pfisterer/llm-management-api/internal/usage"
)

func TestStartCounter(t *testing.T) {
	ctx := context.Background()
	s := service(Running{Server: "other-gpu", Person: "b@dhbw.de", Project: "kurs", Profile: "gpu", GPUClass: "l4-3q", GPUs: 1})
	_, _ = s.create(ctx, Project{Slug: "kurs", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu", "gpu"}, GPUClasses: []string{"l4-3q"},
		Limits: Limits{GPUsPerPerson: 1}, GPUsByClass: map[string]int{"l4-3q": 1}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "git", Members: []Member{{Token: "user:a@dhbw.de"}}, Profiles: []string{"cpu"}, ImageGitURL: "https://github.com/o/r"}, "admin")
	me := []string{"user:a@dhbw.de"}
	for _, r := range []StartRequest{
		{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"},
		{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"},
		{Person: "a@dhbw.de", Project: "nope", Profile: "cpu"},
		{Person: "a@dhbw.de", Project: "kurs", Profile: SplitProfile},
		{Person: "a@dhbw.de", Project: "kurs", Profile: "gpu", GPUs: 1},
		{Person: "a@dhbw.de", Project: "git", Profile: "cpu"},
		{Person: "a@dhbw.de", Project: "kurs", Profile: "whatever"},
		{Person: "a@dhbw.de", Project: "kurs", Profile: SplitProfile, Kernel: true, GPUs: 1, GPUClass: "l4-3q"},
	} {
		_ = s.CheckStart(ctx, me, r)
	}
	s.SetBudgetCheck(func(context.Context, Project, string, string) error { return refuse("Budget aufgebraucht") })
	_ = s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"})
	s.SetBudgetCheck(func(context.Context, Project, string, string) error { return errors.New("db down") })
	_ = s.CheckStart(ctx, me, StartRequest{Person: "a@dhbw.de", Project: "kurs", Profile: "cpu"})

	for _, c := range []struct {
		profile, result string
		want            float64
	}{
		{"cpu", "ok", 2}, {"cpu", "project", 1}, {"cpu-gpukernel", "no_profile", 1}, {"gpu", "gpu_busy", 1}, {"cpu", "image_error", 1},
		{"other", "no_profile", 1}, {"gpu-kernel", "no_profile", 1}, {"cpu", "budget", 1}, {"cpu", "error", 1}, {"gpu", "ok", 0},
	} {
		if got := s.starts.Value(c.profile, c.result); got != c.want {
			t.Errorf("%s/%s: %v, want %v", c.profile, c.result, got, c.want)
		}
	}
	var w promtext.Writer
	s.WriteStartMetrics(&w)
	out := string(w.Bytes())
	for _, want := range []string{"# TYPE aiaas_workspace_starts_total counter\n", `aiaas_workspace_starts_total{profile="cpu",result="ok"} 2` + "\n", `aiaas_workspace_starts_total{profile="gpu",result="material_error"} 0` + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "whatever") || strings.Contains(out, "dhbw.de") {
		t.Fatalf("unknown profile or person in a label:\n%s", out)
	}
}

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := func(back int) time.Time { return usage.Day(now).AddDate(0, 0, -back) }
	u := usage.NewMemoryStore()
	_ = u.Add(ctx, []usage.Row{
		{Day: day(0), Project: "kurs", Person: "a@dhbw.de", Profile: "cpu", ServerHours: 2},
		{Day: day(3), Project: "kurs", Person: "b@dhbw.de", Profile: "gpu", GPUClass: "l4-3q", GPUHours: 5, ServerHours: 5},
		{Day: day(10), Project: "kurs", Person: "b@dhbw.de", Profile: "gpu", GPUClass: "l4-3q", GPUHours: 7},
		{Day: day(1), Project: "anders", Person: "a@dhbw.de", Profile: "cpu", ServerHours: 50},
	})
	cat := catalog
	cat.Bases = []Base{
		{ID: "pytorch-2.10", Family: "pytorch", Name: "PyTorch 2.10", Image: "reg/base-pytorch:v1", Status: "deprecated", ReplacedBy: "pytorch-2.11", EndOfLife: "2027-03-31"},
		{ID: "pytorch-2.11", Family: "pytorch", Name: "PyTorch 2.11", Image: "reg/base-pytorch:v2", Default: true},
		{ID: "python-3.13", Family: "python", Name: "Python", Image: "reg/base-python:v2"},
	}
	s := NewService(NewMemoryStore(), cat, func(context.Context) ([]Running, error) { return nil, nil })
	s.now = func() time.Time { return now }
	s.SetUsage(u)
	ends := now.AddDate(0, 0, 10)
	ended := now.AddDate(0, 0, -10)
	for _, p := range []Project{
		{Slug: "kurs", Name: "Kurs von Max Mustermann", Profiles: []string{"cpu"}, ImageBase: "pytorch-2.10", EndsAt: &ends, StorageGB: 10, LLMMTokens: 5, CreatedAt: now.AddDate(0, -1, 0), Budgets: []Budget{
			{Unit: "gpu_hours", GPUClass: "l4-3q", Window: "7d", Amount: 20, Tree: true},
			{Unit: "server_hours", Window: "24h", Amount: 4, PerPerson: true},
			{Unit: "server_hours", Window: "30d", Amount: 100},
			{Unit: "server_hours", Window: "30d", Amount: 80},
		}},
		{Slug: "img", Profiles: []string{"cpu"}, ProjectImage: "ghcr.io/o/i:1", ImageDigest: "sha256:a", ImageUpdate: "sha256:b"},
		{Slug: "llm", NoWorkspaces: true, LLMMTokens: 1},
		{Slug: "alt", Profiles: []string{"cpu"}, ImageBase: "pytorch-2.10", EndsAt: &ended, StorageGB: 5},
	} {
		if err := s.store.Create(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	s.unpullable.Store("ghcr.io/o/i:1", true)
	if err := s.SetStorageDirs(ctx, []StorageDir{{Dir: "ws-kurs", UsedGiB: 2.5, QuotaGiB: 10}, {Dir: "ws-weg", UsedGiB: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValkeyMemory(ctx, ValkeyMemory{UsedBytes: 1000, MaxBytes: 0}); err != nil {
		t.Fatal(err)
	}

	raw, err := s.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		`aiaas_workspace_projects{state="active"} 2` + "\n",
		`aiaas_workspace_projects{state="llm_only"} 1` + "\n",
		`aiaas_workspace_projects{state="ended"} 1` + "\n",
		`aiaas_llm_projects{state="with_tokens"} 2` + "\n",
		`aiaas_llm_projects{state="without_tokens"} 1` + "\n",
		`aiaas_workspace_project_environments{kind="base"} 1` + "\n",
		`aiaas_workspace_project_environments{kind="image"} 1` + "\n",
		"aiaas_workspace_project_images_unreachable 1\n",
		"aiaas_workspace_project_image_updates_offered 1\n",
		`aiaas_workspace_project_end_timestamp_seconds{project="kurs"} ` + promtext.Float(float64(ends.Unix()+86400)) + "\n",
		`aiaas_workspace_project_budget_limit{project="kurs",unit="gpu_hours",window="7d",gpu_class="l4-3q",source="tree"} 20` + "\n",
		`aiaas_workspace_project_budget_used{project="kurs",unit="gpu_hours",window="7d",gpu_class="l4-3q",source="tree"} 5` + "\n",
		`aiaas_workspace_project_budget_limit{project="kurs",unit="server_hours",window="30d",gpu_class="",source="project"} 80` + "\n",
		`aiaas_workspace_project_budget_used{project="kurs",unit="server_hours",window="30d",gpu_class="",source="project"} 7` + "\n",
		`aiaas_workspace_project_storage_limit_bytes{project="kurs"} 10737418240` + "\n",
		`aiaas_workspace_project_storage_used_bytes{project="kurs"} 2684354560` + "\n",
		`aiaas_workspace_project_storage_limit_bytes{project="alt"} 5368709120` + "\n",
		`aiaas_workspace_metadata_memory_bytes{kind="used"} 1000` + "\n",
		`aiaas_base_projects{base="pytorch-2.10",family="pytorch",status="deprecated"} 1` + "\n",
		`aiaas_base_projects{base="pytorch-2.11",family="pytorch",status="default"} 0` + "\n",
		`aiaas_base_projects{base="python-3.13",family="python",status="current"} 0` + "\n",
		`aiaas_base_end_of_life_timestamp_seconds{base="pytorch-2.10"} 1806451200` + "\n",
		`aiaas_workspace_storage_report_timestamp_seconds{report="dirs"} `,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, not := range []string{"Mustermann", "dhbw.de", `window="24h"`, `project="alt"} 1`, `project="weg"`, `project="llm"`} {
		if strings.Contains(out, not) {
			t.Errorf("%q must not appear in\n%s", not, out)
		}
	}
	// Every series has its family announced once.
	if strings.Count(out, "# TYPE aiaas_workspace_project_budget_used gauge") != 1 {
		t.Fatalf("families:\n%s", out)
	}
}
