package gpu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func TestParseRefAdvertisement(t *testing.T) {
	body := pkt("# service=git-upload-pack\n") + "0000" +
		pkt("1111111111111111111111111111111111111111 HEAD\x00multi_ack symref=HEAD:refs/heads/main agent=git/x\n") +
		pkt("1111111111111111111111111111111111111111 refs/heads/main\n") +
		pkt("2222222222222222222222222222222222222222 refs/heads/dev\n") +
		pkt("3333333333333333333333333333333333333333 refs/tags/v1\n") +
		pkt("4444444444444444444444444444444444444444 refs/tags/v1^{}\n") + "0000"
	refs, err := parseRefAdvertisement(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if refs.DefaultBranch != "main" {
		t.Fatalf("default branch %q", refs.DefaultBranch)
	}
	for _, tc := range []struct{ ref, commit, branch string }{
		{"", "1111111111111111111111111111111111111111", "main"},
		{"HEAD", "1111111111111111111111111111111111111111", "main"},
		{"dev", "2222222222222222222222222222222222222222", "dev"},
		{"v1", "4444444444444444444444444444444444444444", "main"},
		{"abcdef012345", "abcdef012345", "main"},
	} {
		c, b, err := refs.resolve(tc.ref)
		if err != nil || c != tc.commit || b != tc.branch {
			t.Errorf("resolve(%q) = %q %q %v, want %q %q", tc.ref, c, b, err, tc.commit, tc.branch)
		}
	}
	if _, _, err := refs.resolve("nope"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("unknown ref: %v", err)
	}
}

func TestNormaliseRepo(t *testing.T) {
	s := newService(Config{Registry: "reg"}, NewMemoryStore(), nil, nil, nil, nil, zap.NewNop().Sugar())
	u, slug, err := s.normaliseRepo("https://GitHub.com/binder-examples/requirements.git/")
	if err != nil || u != "https://github.com/binder-examples/requirements" || slug != "github-com-binder-examples-requirements" {
		t.Fatalf("got %q %q %v", u, slug, err)
	}
	for _, bad := range []string{"http://github.com/a/b", "https://user:pw@github.com/a/b", "https://evil.example/a/b", "https://github.com/a", "git@github.com:a/b.git"} {
		if _, _, err := s.normaliseRepo(bad); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("%s accepted", bad)
		}
	}
}

type fakeBuilds struct {
	created []string
	status  map[string]jobStatus
	log     string
}

func (f *fakeBuilds) createJob(_ context.Context, _ string, job map[string]any) error {
	name := job["metadata"].(map[string]any)["name"].(string)
	for _, c := range f.created {
		if c == name {
			return errAlreadyExists
		}
	}
	f.created = append(f.created, name)
	return nil
}
func (f *fakeBuilds) jobStatus(_ context.Context, _, name string) (jobStatus, error) {
	if st, ok := f.status[name]; ok {
		return st, nil
	}
	return jobStatus{Active: 1}, nil
}
func (f *fakeBuilds) deleteJob(context.Context, string, string) error { return nil }
func (f *fakeBuilds) jobLog(context.Context, string, string, int) (string, error) {
	if f.log != "" {
		return f.log, nil
	}
	return "log", nil
}

type fakeImages struct{ have map[string]bool }

func (f *fakeImages) exists(_ context.Context, _, repo, tag string) (bool, error) {
	return f.have[repo+":"+tag], nil
}
func (f *fakeImages) newestBuilder(context.Context) (string, error) {
	return "reg/platform/builder:1", nil
}

type fakeHub struct{ started map[string]map[string]any }

func (f *fakeHub) user(context.Context, string) (*hubUser, error) { return &hubUser{}, nil }
func (f *fakeHub) ensureUser(context.Context, string) error       { return nil }
func (f *fakeHub) startServer(_ context.Context, _, server string, o map[string]any) error {
	f.started[server] = o
	return nil
}
func (f *fakeHub) stopServer(context.Context, string, string, bool) error { return nil }

const testBase = "reg/platform/base-pytorch:v2.0.0"

func testService() (*Service, *fakeBuilds, *fakeImages, *fakeHub) {
	b := &fakeBuilds{status: map[string]jobStatus{}}
	i := &fakeImages{have: map[string]bool{}}
	h := &fakeHub{started: map[string]map[string]any{}}
	refs := func(context.Context, string) (gitRefs, error) {
		return gitRefs{Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}, DefaultBranch: "main"}, nil
	}
	s := newService(Config{Registry: "reg", JupyterURL: "https://hub"}, NewMemoryStore(), b, i, h, refs, zap.NewNop().Sugar())
	s.SetBases([]string{"reg/platform/base-python:v2.0.0", testBase})
	return s, b, i, h
}

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

func TestProjectImageBuild(t *testing.T) {
	s, b, i, _ := testService()
	ctx := context.Background()
	var ready []string
	s.SetBuildReady(func(_ context.Context, e Environment) error {
		slug, _ := ProjectOf(e)
		ready = append(ready, slug+" "+e.Image)
		return nil
	})
	got, err := s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	if err != nil || got.Status != "building" || got.Image != "reg/envs/github-com-o-kurs:0123456789ab-pytorch-v2.0.0" {
		t.Fatalf("build %+v %v", got, err)
	}
	if len(b.created) != 1 || len(b.created[0]) > 63 || strings.Contains(b.created[0], ".") {
		t.Fatalf("jobs %v", b.created)
	}
	// Saving again with the same repository keeps the build; the job's success hands the image over.
	if _, err := s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs.git", "", false); err != nil || len(b.created) != 1 {
		t.Fatalf("same repository: %v, jobs %v", err, b.created)
	}
	b.status[b.created[0]] = jobStatus{Succeeded: 1}
	st, err := s.ProjectImageBuild(ctx, "kurs")
	if err != nil || st.Status != "ready" || len(ready) != 1 || ready[0] != "kurs "+got.Image {
		t.Fatalf("after job %+v %v, ready %v", st, err, ready)
	}
	// Another project with the same repository and commit: the image exists, no second build.
	i.have["github-com-o-kurs:0123456789ab-pytorch-v2.0.0"] = true
	other, err := s.BuildProjectImage(ctx, "zweiter", testBase, "https://github.com/o/kurs", "main", false)
	if err != nil || other.Status != "ready" || len(b.created) != 1 {
		t.Fatalf("shared image %+v %v %v", other, err, b.created)
	}
	// A failed build shows the script's own refusal; rebuild builds the same commit again.
	b.log = "Cloning\ndhbw: Das Repository hat ein Dockerfile.\n"
	delete(i.have, "github-com-o-r:0123456789ab-pytorch-v2.0.0")
	f, _ := s.BuildProjectImage(ctx, "dritter", testBase, "https://github.com/o/r", "", false)
	b.status[b.created[len(b.created)-1]] = jobStatus{Failed: 1}
	if st, _ := s.ProjectImageBuild(ctx, "dritter"); st.Status != "failed" || st.Message != "Das Repository hat ein Dockerfile." {
		t.Fatalf("failed %+v", st)
	}
	n := len(b.created)
	if r, err := s.BuildProjectImage(ctx, "dritter", testBase, "https://github.com/o/r", "", true); err != nil || r.Status != "building" || r.Image != f.Image || len(b.created) != n {
		// The fake keeps one job per name, so the forced rebuild reuses the name.
		t.Fatalf("rebuild %+v %v %d", r, err, len(b.created))
	}
	if envs, _ := s.store.List(ctx, projectOwner("dritter")); len(envs) != 1 {
		t.Fatalf("older builds kept: %d", len(envs))
	}
	if err := s.ForgetProjectImage(ctx, "dritter"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.ProjectImageBuild(ctx, "dritter"); st != nil {
		t.Fatalf("forgotten build still there: %+v", st)
	}
}

func TestProjectImagesRebuiltOnNewBase(t *testing.T) {
	s, b, _, _ := testService()
	ctx := context.Background()
	var ready []string
	s.SetBuildReady(func(_ context.Context, e Environment) error {
		ready = append(ready, e.Image)
		return nil
	})
	first, _ := s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	b.status[b.created[0]] = jobStatus{Succeeded: 1}
	if err := s.RebaseProjectImages(ctx); err != nil || len(b.created) != 1 || len(ready) != 1 {
		t.Fatalf("same base: %v, jobs %v, ready %v", err, b.created, ready)
	}
	s.SetBases([]string{"reg/platform/base-pytorch:v2.1.0"})
	if err := s.RebaseProjectImages(ctx); err != nil || len(b.created) != 2 {
		t.Fatalf("new base: %v, jobs %v", err, b.created)
	}
	b.status[b.created[1]] = jobStatus{Succeeded: 1}
	_ = s.RebaseProjectImages(ctx)
	if len(ready) != 2 || ready[1] != "reg/envs/github-com-o-kurs:0123456789ab-pytorch-v2.1.0" || ready[1] == first.Image {
		t.Fatalf("rebuild not handed over: %v", ready)
	}
}

func TestEnsurePrepull(t *testing.T) {
	s, _, _, _ := testService()
	ctx := context.Background()
	f := &fakeDaemonSets{}
	s.pods = f
	if err := s.EnsurePrepull(ctx, []string{"ghcr.io/k/b:1", "ghcr.io/k/a:1", "ghcr.io/k/a:1", ""}); err != nil || f.puts != 1 {
		t.Fatalf("create: %v %d", err, f.puts)
	}
	spec := f.cur["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits := spec["initContainers"].([]any)
	if len(inits) != 4 || inits[0].(map[string]any)["image"] != "ghcr.io/k/a:1" || inits[2].(map[string]any)["image"] != "reg/platform/base-python:v2.0.0" {
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
