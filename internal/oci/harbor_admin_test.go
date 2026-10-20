package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Harbor's listings come in pages of at most 100; repository names with a slash are escaped twice in the path; a delete of something gone counts as done, a refused one is an error; deletes go by digest only.
func TestHarborAdmin(t *testing.T) {
	var deleted []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && p == "/api/v2.0/projects/envs/repositories":
			var page, size int
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			fmt.Sscan(r.URL.Query().Get("page_size"), &size)
			var out []map[string]any
			for i := (page - 1) * size; i < min(page*size, 150); i++ {
				out = append(out, map[string]any{"name": fmt.Sprintf("envs/r%03d", i), "artifact_count": 1, "update_time": "2026-10-01T10:00:00.000Z"})
			}
			if page == 1 {
				out = append(out[:99], map[string]any{"name": "other/x"}) // never this project's: left out
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && p == "/api/v2.0/projects/envs/repositories/team%252Fkurs/artifacts":
			if r.URL.Query().Get("with_tag") != "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `[{"digest":"sha256:a","push_time":"2026-10-01T10:00:00.000Z","pull_time":"0001-01-01T00:00:00.000Z","tags":[{"name":"c1"},{"name":"c2"}]},{"digest":"sha256:b","push_time":"2026-09-01T10:00:00Z","pull_time":"2026-10-02T10:00:00Z","tags":null}]`)
		case r.Method == http.MethodDelete:
			if u, pw, ok := r.BasicAuth(); !ok || u != "robot$envs+cleanup" || pw != "pw" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			switch p {
			case "/api/v2.0/projects/envs/repositories/team%252Fkurs/artifacts/sha256:a", "/api/v2.0/projects/envs/repositories/alt":
				deleted = append(deleted, p)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	ctx := context.Background()
	h := &Harbor{Client: &Client{HTTP: srv.Client()}, Host: host, Auth: &Auth{Username: "robot$envs+cleanup", Password: "pw"}}

	repos, err := h.Repositories(ctx, "envs")
	if err != nil || len(repos) != 149 || repos[0].Name != "r000" || repos[148].Name != "r149" || repos[0].ArtifactCount != 1 || !repos[0].UpdateTime.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("repositories = %d %+v, %v", len(repos), repos[:1], err)
	}
	arts, err := h.Artifacts(ctx, "envs", "team/kurs")
	if err != nil || len(arts) != 2 || arts[0].Digest != "sha256:a" || strings.Join(arts[0].Tags, ",") != "c1,c2" || !arts[0].PullTime.IsZero() || len(arts[1].Tags) != 0 || arts[1].PullTime.IsZero() {
		t.Fatalf("artifacts = %+v, %v", arts, err)
	}
	if _, err := h.Artifacts(ctx, "envs", "weg"); err != ErrNotFound {
		t.Errorf("missing repository: %v", err)
	}

	if err := h.DeleteArtifact(ctx, "envs", "team/kurs", "sha256:a"); err != nil {
		t.Errorf("delete artifact: %v", err)
	}
	if err := h.DeleteArtifact(ctx, "envs", "team/kurs", "sha256:gone"); err != nil {
		t.Errorf("delete of a gone artifact: %v", err)
	}
	if err := h.DeleteArtifact(ctx, "envs", "team/kurs", "c1"); err == nil {
		t.Error("delete by tag accepted")
	}
	if err := h.DeleteRepository(ctx, "envs", "alt"); err != nil {
		t.Errorf("delete repository: %v", err)
	}
	if len(deleted) != 2 {
		t.Errorf("deleted = %v", deleted)
	}
	anon := &Harbor{Client: h.Client, Host: host}
	if err := anon.DeleteRepository(ctx, "envs", "alt"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("delete without the robot: %v", err)
	}
}
