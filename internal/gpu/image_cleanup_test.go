package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/oci"
	"github.com/pfisterer/llm-management-api/internal/promtext"
	"go.uber.org/zap"
)

type fakeArtifact struct {
	digest string
	tags   []string
	push   time.Time
	pull   time.Time
	refs   []string // an index: the digests of its variants
}

// fakeHarbor answers Harbor's v2 API for the project envs from memory: list repositories and artifacts (paged), delete artifacts and repositories (only with the robot's basic auth). Any request outside envs fails the test.
type fakeHarbor struct {
	t       *testing.T
	mu      sync.Mutex
	repos   map[string][]fakeArtifact // name without project
	updated map[string]time.Time
	deletes []string
	failDel map[string]bool // repository → DELETE answers 500
}

func (f *fakeHarbor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v2.0/projects/")
	if !strings.HasPrefix(path, "envs/") {
		f.t.Errorf("request outside envs: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "envs/repositories"), "/")
	// parts: [""] (list), ["", repo], ["", repo, "artifacts"], ["", repo, "artifacts", digest]
	if r.Method == http.MethodDelete {
		if u, p, ok := r.BasicAuth(); !ok || u != "robot$envs+cleanup" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	repo := ""
	if len(parts) > 1 {
		var err error
		if repo, err = url.PathUnescape(parts[1]); err == nil {
			repo, err = url.PathUnescape(repo) // escaped twice
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		type out struct {
			Name          string    `json:"name"`
			ArtifactCount int       `json:"artifact_count"`
			UpdateTime    time.Time `json:"update_time"`
		}
		var list []out
		for _, name := range slices.Sorted(mapKeys(f.repos)) {
			list = append(list, out{Name: "envs/" + name, ArtifactCount: len(f.repos[name]), UpdateTime: f.updated[name]})
		}
		writePage(w, r, list)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "artifacts":
		arts, ok := f.repos[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		type tag struct {
			Name string `json:"name"`
		}
		type ref struct {
			ChildDigest string `json:"child_digest"`
		}
		type out struct {
			Digest     string    `json:"digest"`
			PushTime   time.Time `json:"push_time"`
			PullTime   time.Time `json:"pull_time"`
			Tags       []tag     `json:"tags"`
			References []ref     `json:"references,omitempty"`
		}
		var list []out
		for _, a := range arts {
			o := out{Digest: a.digest, PushTime: a.push, PullTime: a.pull}
			for _, t := range a.tags {
				o.Tags = append(o.Tags, tag{t})
			}
			for _, r := range a.refs {
				o.References = append(o.References, ref{r})
			}
			list = append(list, o)
		}
		writePage(w, r, list)
	case r.Method == http.MethodDelete && len(parts) == 2:
		if f.failDel[repo] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, ok := f.repos[repo]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.repos, repo)
		f.deletes = append(f.deletes, "repo "+repo)
	case r.Method == http.MethodDelete && len(parts) == 4:
		if f.failDel[repo] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		digest, _ := url.PathUnescape(parts[3])
		arts := f.repos[repo]
		i := slices.IndexFunc(arts, func(a fakeArtifact) bool { return a.digest == digest })
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Like Harbor: a variant an index still refers to cannot be deleted.
		if slices.ContainsFunc(arts, func(a fakeArtifact) bool { return slices.Contains(a.refs, digest) }) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.repos[repo] = slices.Delete(arts, i, i+1)
		f.deletes = append(f.deletes, "artifact "+repo+"@"+digest)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func writePage[T any](w http.ResponseWriter, r *http.Request, list []T) {
	var page, size int
	fmt.Sscan(r.URL.Query().Get("page"), &page)
	fmt.Sscan(r.URL.Query().Get("page_size"), &size)
	if page < 1 || size < 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	from, to := min((page-1)*size, len(list)), min(page*size, len(list))
	out := list[from:to]
	if out == nil {
		out = []T{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeHarbor) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.deletes)
	f.deletes = nil
	slices.Sort(out)
	return out
}

const reg = "registry.example"

func TestImageCleanup(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-30*24*time.Hour), now.Add(-24*time.Hour)
	f := &fakeHarbor{t: t, failDel: map[string]bool{}, updated: map[string]time.Time{"leftover": old, "fresh-empty": recent}, repos: map[string][]fakeArtifact{
		// One image shared by two projects (same repository and commit).
		"github-com-a-shared": {{digest: "sha256:s1", tags: []string{"c1-python-1"}, push: old}},
		// A rebuild forgot the older build, the project still runs it until the new one is ready; t2 nobody names.
		"github-com-a-kurs": {
			{digest: "sha256:k1", tags: []string{"c1-python-1"}, push: old},
			{digest: "sha256:k2", tags: []string{"c2-python-1"}, push: old},
			{digest: "sha256:k3", tags: []string{"c3-python-1"}, push: old},
		},
		// Unreferenced, but pushed or pulled within the minimum age.
		"github-com-a-young":  {{digest: "sha256:y1", tags: []string{"c1"}, push: recent}},
		"github-com-a-pulled": {{digest: "sha256:p1", tags: []string{"c1"}, push: old, pull: recent}},
		// Named by digest only (a project pinned to it).
		"github-com-a-pinned": {{digest: "sha256:d1", tags: []string{"moved"}, push: old}},
		// Untagged and unreferenced.
		"github-com-a-gone": {{digest: "sha256:g1", push: old}, {digest: "sha256:g2", tags: []string{"c9"}, push: old}},
		// The build cache: one tag per image repository.
		"buildcache":  {{digest: "sha256:b1", tags: []string{"github-com-a-shared"}, push: old}, {digest: "sha256:b2", tags: []string{"github-com-a-gone"}, push: old}},
		"leftover":    {},
		"fresh-empty": {},
	}}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	harbor := &oci.Harbor{Client: &oci.Client{HTTP: srv.Client()}, Host: host, Auth: &oci.Auth{Username: "robot$envs+cleanup", Password: "s3cret"}}

	store := NewMemoryStore()
	s := newService(Config{Registry: reg}, store, nil, nil, nil, nil, zap.NewNop().Sugar())
	ctx := context.Background()
	envA, _ := store.Save(ctx, Environment{Owner: "project:a", Image: reg + "/envs/github-com-a-shared:c1-python-1", Status: StatusReady})
	envB, _ := store.Save(ctx, Environment{Owner: "project:b", Image: reg + "/envs/github-com-a-shared:c1-python-1", Status: StatusReady})
	_, _ = store.Save(ctx, Environment{Owner: "project:k", Image: reg + "/envs/github-com-a-kurs:c3-python-1", Status: StatusBuilding})
	projectRefs := []string{
		reg + "/envs/github-com-a-kurs:c1-python-1",       // still run by the project
		"other.host/envs/github-com-a-pinned:x@sha256:d1", // another name of the registry: still counts
		reg + "/platform/base-python:1",                   // a base: not in envs, ignored
		reg + "/envs/github-com-a-young",                  // no tag: latest
	}
	var refsErr error
	refs := func(context.Context) ([]string, error) { return projectRefs, refsErr }

	c := s.NewImageCleanup(harbor, ImageCleanupConfig{DryRun: true, MinAge: 7 * 24 * time.Hour}, refs)
	c.now = func() time.Time { return now }

	// Dry run: counts, deletes nothing.
	res := c.Sweep(ctx)
	if got := f.taken(); len(got) != 0 {
		t.Fatalf("dry run deleted %v", got)
	}
	// Orphaned: k2, g1, g2, b2; the leftover repository has no artifacts, the fresh empty one is too young.
	if !res.OK || !res.DryRun || res.Repositories != 9 || res.Artifacts != 11 || res.Referenced != 5 || res.Young != 2 || res.Orphaned != 4 {
		t.Fatalf("dry run = %+v", res)
	}

	c.cfg.DryRun = false
	res = c.Sweep(ctx)
	want := []string{"artifact buildcache@sha256:b2", "artifact github-com-a-kurs@sha256:k2", "repo github-com-a-gone", "repo leftover"}
	if got := f.taken(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if !res.OK || res.Deleted != 4 || res.Failed != 0 {
		t.Errorf("pass = %+v", res)
	}

	// One project goes: the shared image stays for the other; once both are gone, image and cache go.
	_ = store.Delete(ctx, envA.ID)
	c.Sweep(ctx)
	if got := f.taken(); len(got) != 0 {
		t.Fatalf("shared image deleted while another project names it: %v", got)
	}
	_ = store.Delete(ctx, envB.ID)
	// A failing delete is counted and tried again at the next pass; the rest goes on.
	f.failDel["github-com-a-shared"] = true
	res = c.Sweep(ctx)
	if got := f.taken(); !slices.Equal(got, []string{"repo buildcache"}) || res.Failed != 1 || res.OK {
		t.Fatalf("failing pass deleted %v, %+v", got, res)
	}
	f.failDel["github-com-a-shared"] = false
	if c.Sweep(ctx); !slices.Equal(f.taken(), []string{"repo github-com-a-shared"}) {
		t.Fatal("failed delete not retried")
	}

	// References not readable: nothing is deleted.
	f.repos["github-com-a-later"] = []fakeArtifact{{digest: "sha256:l1", tags: []string{"c1"}, push: old}}
	refsErr = errors.New("db down")
	if res := c.Sweep(ctx); res.OK || res.Failed != 1 || len(f.taken()) != 0 {
		t.Fatalf("pass without references = %+v", res)
	}
	refsErr = nil

	// An environment created during the pass (after the first read of the references) keeps its image.
	calls := 0
	c.refs = func(context.Context) ([]string, error) {
		calls++
		if calls == 2 {
			return append(slices.Clone(projectRefs), reg+"/envs/github-com-a-later:c1"), nil
		}
		return projectRefs, nil
	}
	if c.Sweep(ctx); len(f.taken()) != 0 {
		t.Fatal("image taken up during the pass deleted")
	}

	// The last pass shows in the metrics.
	var w promtext.Writer
	s.writeCleanupMetrics(&w)
	if out := string(w.Bytes()); !strings.Contains(out, `aiaas_image_cleanup_last_run_ok{dry_run="false"} 1`) || !strings.Contains(out, `aiaas_image_cleanup_artifacts{state="young",dry_run="false"} 2`) {
		t.Errorf("metrics:\n%s", out)
	}
}

// Without a robot account Harbor refuses deletes: the pass counts the failures and goes on.
func TestImageCleanupWithoutAuth(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	f := &fakeHarbor{t: t, failDel: map[string]bool{}, updated: map[string]time.Time{}, repos: map[string][]fakeArtifact{
		"a": {{digest: "sha256:a1", tags: []string{"c1"}, push: old}},
	}}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	harbor := &oci.Harbor{Client: &oci.Client{HTTP: srv.Client()}, Host: strings.TrimPrefix(srv.URL, "https://")}
	s := newService(Config{Registry: reg}, NewMemoryStore(), nil, nil, nil, nil, zap.NewNop().Sugar())
	res := s.NewImageCleanup(harbor, ImageCleanupConfig{}, nil).Sweep(context.Background())
	if res.OK || res.Failed != 1 || res.Deleted != 0 || len(f.repos["a"]) != 1 {
		t.Fatalf("pass = %+v", res)
	}
}

// Builds for several architectures (multi_arch.go): an index with its variants <tag>-<arch> (Harbor lists tagged variants on their own) stays and goes as one; a variant named by digest keeps its index; the build cache of an architecture stays with its repository.
func TestImageCleanupMultiArch(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	f := &fakeHarbor{t: t, failDel: map[string]bool{}, updated: map[string]time.Time{}, repos: map[string][]fakeArtifact{
		"github-com-a-multi": {
			// Named by its tag: index and variants stay.
			{digest: "sha256:i1", tags: []string{"c1-pytorch-1"}, refs: []string{"sha256:v1", "sha256:v2"}, push: old},
			{digest: "sha256:v1", tags: []string{"c1-pytorch-1-amd64"}, push: old},
			{digest: "sha256:v2", tags: []string{"c1-pytorch-1-arm64"}, push: old},
			// Nobody names it any more: the index first, then its tagged variant (its untagged one Harbor takes with the index).
			{digest: "sha256:v3", tags: []string{"c0-pytorch-1-amd64"}, push: old},
			{digest: "sha256:i0", tags: []string{"c0-pytorch-1"}, refs: []string{"sha256:v3", "sha256:v4"}, push: old},
		},
		// A project pinned to a variant's digest: its index (and so the variant) stays.
		"github-com-a-pinvar": {{digest: "sha256:i2", tags: []string{"t"}, refs: []string{"sha256:x1", "sha256:x2"}, push: old}},
		"buildcache": {
			{digest: "sha256:b1", tags: []string{"github-com-a-multi-course"}, push: old},
			{digest: "sha256:b2", tags: []string{"github-com-a-multi-arm64-course"}, push: old},
			{digest: "sha256:b3", tags: []string{"github-com-a-gone-arm64-course"}, push: old},
		},
	}}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	harbor := &oci.Harbor{Client: &oci.Client{HTTP: srv.Client()}, Host: strings.TrimPrefix(srv.URL, "https://"), Auth: &oci.Auth{Username: "robot$envs+cleanup", Password: "s3cret"}}
	store := NewMemoryStore()
	s := newService(Config{Registry: reg}, store, nil, nil, nil, nil, zap.NewNop().Sugar())
	ctx := context.Background()
	_, _ = store.Save(ctx, Environment{Owner: "project:a", Image: reg + "/envs/github-com-a-multi:c1-pytorch-1", Platforms: "amd64,arm64", Status: StatusReady})
	refs := func(context.Context) ([]string, error) {
		return []string{reg + "/envs/github-com-a-pinvar@sha256:x2"}, nil
	}
	c := s.NewImageCleanup(harbor, ImageCleanupConfig{MinAge: 7 * 24 * time.Hour}, refs)
	c.now = func() time.Time { return now }
	res := c.Sweep(ctx)
	want := []string{"artifact buildcache@sha256:b3", "artifact github-com-a-multi@sha256:i0", "artifact github-com-a-multi@sha256:v3"}
	if got := f.taken(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if !res.OK || res.Failed != 0 || res.Referenced != 6 || res.Orphaned != 3 || res.Deleted != 3 {
		t.Fatalf("pass = %+v", res)
	}
}
