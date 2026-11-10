package gpu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testRegistry is the registry client against srv (a TLS server: the registry is always asked over https).
func testRegistry(srv *httptest.Server) *registry {
	r := newRegistry(strings.TrimPrefix(srv.URL, "https://"))
	r.harbor.Client.HTTP = srv.Client()
	return r
}

// exists asks Harbor for the artifact, the repository's slashes escaped twice: 200 = there, 404 = not, anything else an error.
func TestRegistryExists(t *testing.T) {
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		switch {
		case strings.HasSuffix(r.URL.Path, "/there"):
			_, _ = w.Write([]byte(`{"digest":"sha256:1"}`))
		case strings.HasSuffix(r.URL.Path, "/gone"):
			http.NotFound(w, r)
		default:
			http.Error(w, "kaputt", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	r, ctx := testRegistry(srv), context.Background()
	if ok, err := r.exists(ctx, "envs", "github-com-o/kurs", "there"); !ok || err != nil {
		t.Fatalf("200: %v %v", ok, err)
	}
	if want := "/api/v2.0/projects/envs/repositories/github-com-o%252Fkurs/artifacts/there"; paths[0] != want {
		t.Fatalf("path %s, want %s", paths[0], want)
	}
	if ok, err := r.exists(ctx, "envs", "kurs", "gone"); ok || err != nil {
		t.Fatalf("404: %v %v", ok, err)
	}
	if ok, err := r.exists(ctx, "envs", "kurs", "broken"); ok || err == nil {
		t.Fatalf("500: %v %v", ok, err)
	}
}

// The newest builder is asked once per few minutes; a failed answer is an error, not a builder.
func TestNewestBuilderCached(t *testing.T) {
	var calls atomic.Int32
	fail := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail {
			http.Error(w, "kaputt", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/api/v2.0/projects/platform/repositories/repo2docker-buildkit/artifacts" || r.URL.Query().Get("sort") != "-push_time" || r.URL.Query().Get("with_tag") != "true" {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"tags":[{"name":"v2-amd64"},{"name":"v2"}]}]`))
	}))
	defer srv.Close()
	r, ctx := testRegistry(srv), context.Background()
	for range 3 {
		if got, err := r.newestBuilder(ctx); err != nil || got != r.host+"/platform/repo2docker-buildkit:v2" {
			t.Fatalf("builder %q %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d calls, want 1 (cached)", calls.Load())
	}
	fail = true
	r2 := testRegistry(srv)
	if got, err := r2.newestBuilder(ctx); err == nil || got != "" {
		t.Fatalf("500: %q %v", got, err)
	}
}
