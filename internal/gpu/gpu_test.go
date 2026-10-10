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

func testService() (*Service, *fakeBuilds, *fakeImages, *fakeHub) {
	b := &fakeBuilds{status: map[string]jobStatus{}}
	i := &fakeImages{have: map[string]bool{}}
	h := &fakeHub{started: map[string]map[string]any{}}
	refs := func(context.Context, string) (gitRefs, error) {
		return gitRefs{Refs: map[string]string{"refs/heads/main": "0123456789abcdef0123456789abcdef01234567"}, DefaultBranch: "main"}, nil
	}
	return newService(Config{Registry: "reg", JupyterURL: "https://hub"}, NewMemoryStore(), b, i, h, refs, zap.NewNop().Sugar()), b, i, h
}

func TestCreateBuildsOnceAndShares(t *testing.T) {
	s, b, i, h := testService()
	ctx := context.Background()
	a, err := s.Create(ctx, "a@dhbw.de", "https://github.com/o/r", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusBuilding || a.Image != "reg/envs/github-com-o-r:0123456789ab" || a.Branch != "main" || a.Name != "r" {
		t.Fatalf("unexpected %+v", a)
	}
	// Same repository and commit for someone else: joins the running build.
	bb, err := s.Create(ctx, "b@dhbw.de", "https://github.com/o/r", "main", "")
	if err != nil || bb.Status != StatusBuilding || len(b.created) != 1 {
		t.Fatalf("second create %+v %v, jobs %v", bb, err, b.created)
	}
	if _, err := s.Start(ctx, a, "a@dhbw.de", false); !errors.Is(err, ErrNotReady) {
		t.Fatalf("start before ready: %v", err)
	}
	b.status[a.JobName] = jobStatus{Succeeded: 1}
	got, err := s.Get(ctx, a.ID, "a@dhbw.de", false)
	if err != nil || got.Status != StatusReady {
		t.Fatalf("after build %+v %v", got, err)
	}
	if _, err := s.Get(ctx, a.ID, "b@dhbw.de", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign get: %v", err)
	}
	u, err := s.Start(ctx, got, "a@dhbw.de", true)
	if err != nil || !strings.HasPrefix(u, "https://hub/user/a@dhbw.de/r-") {
		t.Fatalf("start %q %v", u, err)
	}
	opts := h.started[ServerName(got)]
	if opts["profile"] != "git-gpu" || opts["image--unlisted-choice"] != got.Image {
		t.Fatalf("hub options %v", opts)
	}
	// Image already in Harbor: no build at all.
	i.have["github-com-o-other:0123456789ab"] = true
	c, err := s.Create(ctx, "a@dhbw.de", "https://github.com/o/other", "", "Mein Kurs")
	if err != nil || c.Status != StatusReady || len(b.created) != 1 || c.Name != "Mein Kurs" {
		t.Fatalf("existing image %+v %v %v", c, err, b.created)
	}
}

func TestFailedBuild(t *testing.T) {
	s, b, _, _ := testService()
	ctx := context.Background()
	e, _ := s.Create(ctx, "a@dhbw.de", "https://github.com/o/r", "", "")
	b.status[e.JobName] = jobStatus{Failed: 1}
	list, err := s.List(ctx, "a@dhbw.de")
	if err != nil || len(list) != 1 || list[0].Status != StatusFailed || list[0].Message == "" {
		t.Fatalf("%+v %v", list, err)
	}
}
