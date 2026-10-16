package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := map[string]Ref{
		"ubuntu":                          {"registry-1.docker.io", "library/ubuntu", "latest"},
		"jupyter/scipy-notebook:2026":     {"registry-1.docker.io", "jupyter/scipy-notebook", "2026"},
		"ghcr.io/org/img:v1":              {"ghcr.io", "org/img", "v1"},
		"localhost:5000/img":              {"localhost:5000", "img", "latest"},
		"harbor.example/envs/x@sha256:ab": {"harbor.example", "envs/x", "sha256:ab"},
		"ghcr.io/org/img:v1@sha256:cd":    {"ghcr.io", "org/img", "sha256:cd"},
	}
	for in, want := range cases {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseRef("a b"); err == nil {
		t.Error("blank inside accepted")
	}
}

// A registry with an index, token auth on every path and one public repository.
func TestInspect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.URL.Query().Get("scope") != "repository:org/course:pull" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"token":"t1"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t1" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:org/course:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/org/course/manifests/v1", "/v2/org/course/manifests/sha256:index":
			w.Header().Set("Docker-Content-Digest", "sha256:index")
			fmt.Fprint(w, `{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`)
		case "/v2/org/course/manifests/arm-only":
			fmt.Fprint(w, `{"config":{"digest":"sha256:armcfg"}}`)
		case "/v2/org/course/blobs/sha256:armcfg":
			fmt.Fprint(w, `{"architecture":"arm64","config":{}}`)
		case "/v2/org/course/manifests/sha256:amd":
			fmt.Fprint(w, `{"config":{"digest":"sha256:cfg"}}`)
		case "/v2/org/course/blobs/sha256:cfg":
			fmt.Fprint(w, `{"config":{"Labels":{"cloud.dhbw.base":"notebook-v1"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := &Client{HTTP: srv.Client()}

	img, err := c.Inspect(context.Background(), host+"/org/course:v1")
	if err != nil || img.Labels["cloud.dhbw.base"] != "notebook-v1" || img.Digest != "sha256:index" || !slices.Equal(img.Archs, []string{"amd64", "arm64"}) {
		t.Fatalf("image = %+v, %v", img, err)
	}
	// A single-platform image names its architecture in its config.
	if one, err := c.Inspect(context.Background(), host+"/org/course:arm-only"); err != nil || !slices.Equal(one.Archs, []string{"arm64"}) {
		t.Fatalf("single platform = %+v, %v", one, err)
	}
	if got := img.Pinned(host + "/org/course:v1"); got != host+"/org/course:v1@sha256:index" {
		t.Errorf("pinned = %s", got)
	}
	// A pinned reference is read by its digest and keeps it.
	if again, err := c.Inspect(context.Background(), img.Pinned(host+"/org/course:v1")); err != nil || again.Digest != "sha256:index" {
		t.Errorf("by digest = %+v, %v", again, err)
	}
	if _, err := c.Inspect(context.Background(), host+"/org/course:v2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v", err)
	}
	if _, err := c.Inspect(context.Background(), host+"/org/private:v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("private repository: %v", err)
	}
}
