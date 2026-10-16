package workspace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func versionedService() *Service {
	s := service()
	s.cfg.Bases = []Base{
		{ID: "python-3.13", Family: "python", Name: "Python 3.13", Image: "reg/platform/base-python:v3"},
		{ID: "pytorch-2.11-cu13", Family: "pytorch", Name: "PyTorch 2.11 · CUDA 13", Image: "reg/platform/base-pytorch:v3", Default: true},
		{ID: "pytorch-2.8-cu12", Family: "pytorch", Name: "PyTorch 2.8 · CUDA 12", Image: "reg/platform/base-pytorch-2.8:v3", Status: "deprecated", ReplacedBy: "pytorch-2.11-cu13", EndOfLife: "2026-12-31"},
	}
	return s
}

// Projects from before versions get the version they ran; a deprecated version cannot be chosen but stays where it is; an unknown version runs as its family's current one.
func TestBaseVersions(t *testing.T) {
	ctx := context.Background()
	s := versionedService()
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	for slug, base := range map[string]string{"leer": "", "familie": "pytorch", "alt": "pytorch-2.8-cu12", "weg": "pytorch-2.5-cu11"} {
		if err := s.store.Create(ctx, &Project{Slug: slug, Profiles: []string{"cpu"}, Members: mgr, ImageBase: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MigrateBases(ctx); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{"leer": "pytorch-2.11-cu13", "familie": "pytorch-2.11-cu13", "alt": "pytorch-2.8-cu12", "weg": "pytorch-2.11-cu13"} {
		if p := mustGet(t, s, slug); p.ImageBase != want {
			t.Errorf("%s: %s, want %s", slug, p.ImageBase, want)
		}
	}
	// Choosing the deprecated version is refused; a project on it saves other changes.
	if _, err := s.Update(ctx, Project{Slug: "leer", Members: mgr, ImageBase: "pytorch-2.8-cu12"}, "a@dhbw.de", false); !strings.Contains(reason(err), "läuft aus") {
		t.Fatalf("deprecated chosen: %v", err)
	}
	if p, err := s.Update(ctx, Project{Slug: "alt", Members: mgr, ImageBase: "pytorch-2.8-cu12", Description: "neu"}, "a@dhbw.de", false); err != nil || p.ImageBase != "pytorch-2.8-cu12" {
		t.Fatalf("project on the deprecated version: %+v %v", p, err)
	}
	// The catalogue counts the projects of each version and their latest end.
	end := time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)
	p := mustGet(t, s, "alt")
	p.EndsAt = &end
	_ = s.store.Update(ctx, &p)
	for _, b := range s.Catalog(ctx).Bases {
		switch b.ID {
		case "pytorch-2.8-cu12":
			if b.Projects != 1 || b.LastEnd == nil || !b.LastEnd.Equal(end) {
				t.Errorf("deprecated usage %+v", b)
			}
		case "pytorch-2.11-cu13":
			if b.Projects != 3 || b.LastEnd != nil {
				t.Errorf("current usage %+v", b)
			}
		}
	}
	// Before the end of life nothing moves, after it the project goes to the successor.
	s.now = func() time.Time { return time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC) }
	if err := s.RetireBases(ctx); err != nil || mustGet(t, s, "alt").ImageBase != "pytorch-2.8-cu12" {
		t.Fatalf("moved on its last day: %v", err)
	}
	s.now = func() time.Time { return time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC) }
	if err := s.RetireBases(ctx); err != nil || mustGet(t, s, "alt").ImageBase != "pytorch-2.11-cu13" {
		t.Fatalf("not moved after its end of life: %v", err)
	}
}
