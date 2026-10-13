package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
func TestLabels(t *testing.T) {
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
		case "/v2/org/course/manifests/v1":
			fmt.Fprint(w, `{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`)
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

	labels, err := c.Labels(context.Background(), host+"/org/course:v1")
	if err != nil || labels["cloud.dhbw.base"] != "notebook-v1" {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	if _, err := c.Labels(context.Background(), host+"/org/course:v2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v", err)
	}
	if _, err := c.Labels(context.Background(), host+"/org/private:v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("private repository: %v", err)
	}
}
