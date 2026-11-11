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
			fmt.Fprint(w, `{"schemaVersion":2,"config":{"digest":"sha256:cfg","size":7023},"layers":[{"digest":"sha256:l1","size":30000000},{"digest":"sha256:l2","size":1970000000}]}`)
		case "/v2/org/course/blobs/sha256:cfg":
			fmt.Fprint(w, `{"architecture":"amd64","os":"linux","config":{"User":"1000","Env":["PATH=/opt/conda/bin:/usr/bin","HOME=/home/jovyan"],"Entrypoint":["tini","-g","--"],"Cmd":["start-notebook.py"],"WorkingDir":"/home/jovyan","Labels":{"cloud.dhbw.base":"notebook-v1"}}}`)
		case "/v2/org/course/manifests/windows":
			fmt.Fprint(w, `{"config":{"digest":"sha256:wincfg"}}`)
		case "/v2/org/course/blobs/sha256:wincfg":
			fmt.Fprint(w, `{"architecture":"amd64","os":"windows","config":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := &Client{HTTP: srv.Client()}

	img, err := c.Inspect(context.Background(), host+"/org/course:v1", nil)
	if err != nil || img.Labels["cloud.dhbw.base"] != "notebook-v1" || img.Digest != "sha256:index" || !slices.Equal(img.Archs, []string{"amd64", "arm64"}) {
		t.Fatalf("image = %+v, %v", img, err)
	}
	// The run settings and the download of the amd64 variant (its layers, not the config).
	if img.Size != 2_000_000_000 || img.User != "1000" || !slices.Equal(img.Entrypoint, []string{"tini", "-g", "--"}) || !slices.Equal(img.Cmd, []string{"start-notebook.py"}) || img.WorkingDir != "/home/jovyan" || !slices.Contains(img.Env, "HOME=/home/jovyan") {
		t.Errorf("config = %+v", img)
	}
	if _, err := c.Inspect(context.Background(), host+"/org/course:windows", nil); err == nil || !strings.Contains(err.Error(), "no linux variant") {
		t.Errorf("windows image: %v", err)
	}
	// A single-platform image names its architecture in its config.
	if one, err := c.Inspect(context.Background(), host+"/org/course:arm-only", nil); err != nil || !slices.Equal(one.Archs, []string{"arm64"}) {
		t.Fatalf("single platform = %+v, %v", one, err)
	}
	if got := img.Pinned(host + "/org/course:v1"); got != host+"/org/course:v1@sha256:index" {
		t.Errorf("pinned = %s", got)
	}
	// A pinned reference is read by its digest and keeps it.
	if again, err := c.Inspect(context.Background(), img.Pinned(host+"/org/course:v1"), nil); err != nil || again.Digest != "sha256:index" {
		t.Errorf("by digest = %+v, %v", again, err)
	}
	if _, err := c.Inspect(context.Background(), host+"/org/course:v2", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v", err)
	}
	if _, err := c.Inspect(context.Background(), host+"/org/private:v1", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("private repository: %v", err)
	}
}

// A private image: the token endpoint hands out a token only for the right user and password; a registry that asks for Basic gets them directly.
func TestInspectAuth(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		authorized := ok && u == "dozent" && p == "geheim"
		switch {
		case r.URL.Path == "/token":
			if !authorized {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"token":"t2"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/basic/"):
			if !authorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			serveOne(w, r)
		case r.Header.Get("Authorization") != "Bearer t2":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:org/private:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			serveOne(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := &Client{HTTP: srv.Client()}
	good := &Auth{Username: "dozent", Password: "geheim"}
	for _, repo := range []string{"org/private", "basic/private"} {
		if _, err := c.Inspect(context.Background(), host+"/"+repo+":v1", nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s anonymously: %v", repo, err)
		}
		if _, err := c.Inspect(context.Background(), host+"/"+repo+":v1", &Auth{Username: "dozent", Password: "falsch"}); err == nil {
			t.Errorf("%s with a wrong password", repo)
		}
		if img, err := c.Inspect(context.Background(), host+"/"+repo+":v1", good); err != nil || img.Digest != "sha256:one" {
			t.Errorf("%s with access: %+v %v", repo, img, err)
		}
	}
}

// serveOne answers for a single-platform image: manifest sha256:one with config sha256:c1.
func serveOne(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/manifests/v1"):
		w.Header().Set("Docker-Content-Digest", "sha256:one")
		fmt.Fprint(w, `{"config":{"digest":"sha256:c1"}}`)
	case strings.HasSuffix(r.URL.Path, "/blobs/sha256:c1"):
		fmt.Fprint(w, `{"architecture":"amd64","config":{}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
