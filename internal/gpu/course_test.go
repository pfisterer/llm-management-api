package gpu

import (
	"context"
	"strings"
	"testing"
)

type fakeDaemonSets struct {
	fakePods
	cur  map[string]any
	puts int
}

func (f *fakeDaemonSets) getDaemonSet(context.Context, string, string) (map[string]any, error) {
	if f.cur == nil {
		return nil, ErrNotFound
	}
	return f.cur, nil
}

func (f *fakeDaemonSets) putDaemonSet(_ context.Context, _, _ string, ds map[string]any, _ bool) error {
	f.cur, f.puts = ds, f.puts+1
	return nil
}

func TestCourseImageRebuiltOnNewBase(t *testing.T) {
	s, b, i, _ := testService()
	ctx := context.Background()
	if _, err := s.Create(ctx, "a@dhbw.de", "https://github.com/o/kurs", "", "", true); err == nil {
		t.Fatal("course image without a configured base")
	}
	s.cfg.CourseBase = "reg/platform/notebook-base:v1.1.0"
	e, err := s.Create(ctx, "a@dhbw.de", "https://github.com/o/kurs", "", "", true)
	if err != nil || e.Image != "reg/envs/github-com-o-kurs:0123456789ab-v1.1.0" || e.Base != s.cfg.CourseBase || e.Status != StatusBuilding {
		t.Fatalf("course env %+v %v", e, err)
	}
	if len(e.JobName) > 63 || strings.Contains(e.JobName, ".") {
		t.Fatalf("job name %q", e.JobName)
	}
	b.status[e.JobName] = jobStatus{Succeeded: 1}
	var moved [][2]string
	s.SetImageReplaced(func(_ context.Context, old, new string) error {
		moved = append(moved, [2]string{old, new})
		return nil
	})
	// Same base: nothing to do but follow the build.
	if err := s.RebaseCourseImages(ctx); err != nil || len(b.created) != 1 {
		t.Fatalf("same base: %v, jobs %v", err, b.created)
	}
	// New release of notebook-base: a rebuild, the projects follow once it is ready.
	s.cfg.CourseBase = "reg/platform/notebook-base:v1.2.0"
	if err := s.RebaseCourseImages(ctx); err != nil || len(b.created) != 2 {
		t.Fatalf("new base: %v, jobs %v", err, b.created)
	}
	got, _ := s.store.Get(ctx, e.ID)
	if got.Image != "reg/envs/github-com-o-kurs:0123456789ab-v1.2.0" || got.Replaces != e.Image || len(moved) != 0 {
		t.Fatalf("rebuilding %+v, moved %v", got, moved)
	}
	b.status[got.JobName] = jobStatus{Succeeded: 1}
	_ = s.RebaseCourseImages(ctx)
	got, _ = s.store.Get(ctx, e.ID)
	if got.Status != StatusReady || got.Replaces != "" || len(moved) != 1 || moved[0] != [2]string{e.Image, got.Image} {
		t.Fatalf("after rebuild %+v, moved %v", got, moved)
	}
	// Ordinary environments are left alone.
	i.have["github-com-o-r:0123456789ab"] = true
	plain, _ := s.Create(ctx, "a@dhbw.de", "https://github.com/o/r", "", "", false)
	s.cfg.CourseBase = "reg/platform/notebook-base:v1.3.0"
	_ = s.RebaseCourseImages(ctx)
	if p, _ := s.store.Get(ctx, plain.ID); p.Image != plain.Image {
		t.Fatalf("plain environment rebuilt: %+v", p)
	}
}

func TestEnsurePrepull(t *testing.T) {
	s, _, _, _ := testService()
	ctx := context.Background()
	f := &fakeDaemonSets{}
	s.pods = f
	s.cfg.CourseBase = "reg/platform/notebook-base:v1.1.0"
	if err := s.EnsurePrepull(ctx, []string{"ghcr.io/k/b:1", "ghcr.io/k/a:1", "ghcr.io/k/a:1", ""}); err != nil || f.puts != 1 {
		t.Fatalf("create: %v %d", err, f.puts)
	}
	spec := f.cur["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits := spec["initContainers"].([]any)
	if len(inits) != 3 || inits[0].(map[string]any)["image"] != s.cfg.CourseBase || inits[1].(map[string]any)["image"] != "ghcr.io/k/a:1" {
		t.Fatalf("init containers %v", inits)
	}
	f.cur["metadata"].(map[string]any)["resourceVersion"] = "7"
	if err := s.EnsurePrepull(ctx, []string{"ghcr.io/k/a:1", "ghcr.io/k/b:1"}); err != nil || f.puts != 1 {
		t.Fatalf("unchanged list changed the DaemonSet: %v %d", err, f.puts)
	}
	if err := s.EnsurePrepull(ctx, []string{"ghcr.io/k/a:1"}); err != nil || f.puts != 2 || f.cur["metadata"].(map[string]any)["resourceVersion"] != "7" {
		t.Fatalf("update: %v %d %v", err, f.puts, f.cur["metadata"])
	}
}
