package gpu

import (
	"slices"

	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/workspace"

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
	jobs    map[string]map[string]any
	deleted []string
	status  map[string]jobStatus
	log     string
	logs    map[string]string // per job, before log
}

func (f *fakeBuilds) createJob(_ context.Context, _ string, job map[string]any) error {
	name := job["metadata"].(map[string]any)["name"].(string)
	for _, c := range f.created {
		if c == name {
			return errAlreadyExists
		}
	}
	f.created = append(f.created, name)
	if f.jobs == nil {
		f.jobs = map[string]map[string]any{}
	}
	f.jobs[name] = job
	return nil
}

// jobStatus: a job not created is not found, one created runs until the test sets its status.
func (f *fakeBuilds) jobStatus(_ context.Context, _, name string) (jobStatus, error) {
	if st, ok := f.status[name]; ok {
		return st, nil
	}
	if !slices.Contains(f.created, name) {
		return jobStatus{}, ErrNotFound
	}
	return jobStatus{Active: 1}, nil
}
func (f *fakeBuilds) deleteJob(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeBuilds) jobState(context.Context, string, string) (string, *time.Time, error) {
	return "Läuft.", nil, nil
}

func (f *fakeBuilds) jobLog(_ context.Context, _, name string, _ int) (string, error) {
	if l, ok := f.logs[name]; ok {
		return l, nil
	}
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
func (f *fakeHub) progress(context.Context, string, string) (Progress, error) {
	return Progress{Events: []ProgressEvent{}}, nil
}

const testBase = "reg/platform/base-pytorch:v2.0.0"

func testService() (*Service, *fakeBuilds, *fakeImages, *fakeHub) {
	b := &fakeBuilds{status: map[string]jobStatus{}}
	i := &fakeImages{have: map[string]bool{}}
	h := &fakeHub{started: map[string]map[string]any{}}
	refs := func(context.Context, string, *workspace.Basic) (gitRefs, error) {
		return gitRefs{Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}, DefaultBranch: "main"}, nil
	}
	s := newService(Config{Registry: "reg", JupyterURL: "https://hub"}, NewMemoryStore(), b, i, h, refs, zap.NewNop().Sugar())
	s.SetBases([]workspace.Base{{Image: "reg/platform/base-python:v2.0.0"}, {Image: testBase}})
	return s, b, i, h
}

type fakeDaemonSets struct {
	fakePods
	all  map[string]map[string]any
	cur  map[string]any // the amd64 one, PrepullName
	puts int
}

func (f *fakeDaemonSets) getDaemonSet(_ context.Context, _, name string) (map[string]any, error) {
	if d, ok := f.all[name]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}

func (f *fakeDaemonSets) putDaemonSet(_ context.Context, _, name string, ds map[string]any, _ bool) error {
	if f.all == nil {
		f.all = map[string]map[string]any{}
	}
	f.all[name], f.puts = ds, f.puts+1
	if name == PrepullName {
		f.cur = ds
	}
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
	s.SetBases([]workspace.Base{{Image: "reg/platform/base-pytorch:v2.1.0"}})
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
	both := []string{"amd64", "arm64"}
	base := workspace.ArchImage{Image: "reg/platform/base-python:v2", Archs: both, Base: true}
	images := []workspace.ArchImage{base, {Image: "ghcr.io/k/b:1", Archs: []string{"amd64"}}, {Image: "ghcr.io/k/a:1", Archs: both}, {Image: "ghcr.io/k/a:1", Archs: both}, {Image: "ghcr.io/k/arm:1", Archs: []string{"arm64"}}, {}}
	if err := s.EnsurePrepull(ctx, images); err != nil || f.puts != 2 {
		t.Fatalf("create: %v %d", err, f.puts)
	}
	// One DaemonSet per architecture, each with the images of its architecture: the first base holds the pod, every other image is an image volume the container mounts (and never runs).
	for name, want := range map[string][]string{PrepullName: {"ghcr.io/k/a:1", "ghcr.io/k/b:1"}, PrepullName + "-arm64": {"ghcr.io/k/a:1", "ghcr.io/k/arm:1"}} {
		d := f.all[name]
		spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		hold := spec["containers"].([]any)[0].(map[string]any)
		var refs []string
		for _, v := range spec["volumes"].([]any) {
			refs = append(refs, v.(map[string]any)["image"].(map[string]any)["reference"].(string))
		}
		if hold["image"] != base.Image || spec["initContainers"] != nil || !slices.Equal(refs, want) || len(hold["volumeMounts"].([]any)) != len(refs) {
			t.Fatalf("%s: hold %v, volumes %v", name, hold["image"], refs)
		}
	}
	f.cur["metadata"].(map[string]any)["resourceVersion"] = "7"
	if err := s.EnsurePrepull(ctx, images); err != nil || f.puts != 2 {
		t.Fatalf("unchanged list changed the DaemonSet: %v %d", err, f.puts)
	}
	// Without a base for arm64 there is nothing to hold its pod: no arm64 DaemonSet is written.
	if err := s.EnsurePrepull(ctx, []workspace.ArchImage{{Image: base.Image, Archs: []string{"amd64"}, Base: true}, {Image: "ghcr.io/k/arm:1", Archs: []string{"arm64"}}}); err != nil || f.puts != 3 {
		t.Fatalf("update: %v %d", err, f.puts)
	}
}

type runningHub struct {
	fakeHub
	running map[string]HubServer
}

func (f *runningHub) user(context.Context, string) (*hubUser, error) {
	return &hubUser{Servers: f.running}, nil
}

func TestStartServerNames(t *testing.T) {
	s, _, _, _ := testService()
	h := &runningHub{fakeHub: fakeHub{started: map[string]map[string]any{}}, running: map[string]HubServer{"ml-gpu": {Ready: true}}}
	s.servers = h
	srv, err := s.StartServer(context.Background(), "a@b.de", StartOptions{Project: "ml", Profile: "gpu", GPUClass: "general-medium"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Name != "ml-gpu-2" || srv.URL != "https://hub/user/a@b.de/ml-gpu-2/" {
		t.Fatalf("server: %+v", srv)
	}
	o := h.started["ml-gpu-2"]
	if o["profile"] != "gpu" || o["project"] != "ml" || o["gpu_class"] != "general-medium" {
		t.Fatalf("options: %v", o)
	}
	if _, err := s.StartServer(context.Background(), "a@b.de", StartOptions{Project: "ml", Profile: "cpu"}); err != nil {
		t.Fatal(err)
	}
	if _, has := h.started["ml-cpu"]["gpu_class"]; has {
		t.Fatal("cpu start carries a GPU class")
	}
}

func TestCheckGitBranch(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := testService()
	s.cfg.GitHosts = []string{"github.com"}
	if u, err := s.CheckGitBranch(ctx, "https://github.com/o/kurs.git/", "", nil); err != nil || u != "https://github.com/o/kurs" {
		t.Fatalf("default branch: %q %v", u, err)
	}
	if _, err := s.CheckGitBranch(ctx, "https://github.com/o/kurs", "main", nil); err != nil {
		t.Fatalf("branch: %v", err)
	}
	for _, bad := range [][2]string{{"https://github.com/o/kurs", "v1"}, {"https://gitlab.example/o/kurs", ""}, {"http://github.com/o/kurs", ""}} {
		if _, err := s.CheckGitBranch(ctx, bad[0], bad[1], nil); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// A wildcard entry allows the domain and every host under it, nothing that merely ends in the same letters.
func TestGitHostWildcard(t *testing.T) {
	s, _, _, _ := testService()
	s.cfg.GitHosts = []string{"github.com", "*.dhbw.de"}
	for host, ok := range map[string]bool{"github.com": true, "dhbw.de": true, "gitlab.dhbw.de": true, "git.mosbach.dhbw.de": true, "evil-dhbw.de": false, "dhbw.de.evil.com": false, "gitlab.com": false} {
		if _, _, err := s.normaliseRepo("https://" + host + "/o/kurs"); (err == nil) != ok {
			t.Errorf("%s: %v", host, err)
		}
	}
}
