package webserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/workspace"
	"go.uber.org/zap"
)

// failingStore makes every read fail, as a lost database would.
type failingStore struct{ fleet.Store }

func (failingStore) List(context.Context) ([]fleet.Peer, error) { return nil, errors.New("db down") }

func TestMetricsRouter(t *testing.T) {
	cfg, wg, err := fleet.ParseConfig(`{"enabled":true}`, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	get := func(store fleet.Store, path string) *httptest.ResponseRecorder {
		h := MetricsRouter(MetricsOptions{Fleet: fleet.NewService(cfg, wg, store, fleet.NewScripts(t.TempDir())), Log: zap.NewNop().Sugar(), DevMode: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get(fleet.NewMemoryStore(), "/metrics")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") || !strings.Contains(rec.Body.String(), "aiaas_fleet_enabled 1\n") {
		t.Fatalf("metrics: %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// Nothing else is served on this listener.
	if rec := get(fleet.NewMemoryStore(), "/fleet/peers"); rec.Code != http.StatusNotFound {
		t.Fatalf("other routes must not exist here: %d", rec.Code)
	}
	// A store error fails the scrape instead of serving a page without machines.
	if rec := get(failingStore{}, "/metrics"); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "aiaas_fleet_machines") {
		t.Fatalf("store error: %d %s", rec.Code, rec.Body.String())
	}
}

// failingProjects makes the projects unreadable, as a lost database would.
type failingProjects struct{ workspace.Store }

func (failingProjects) List(context.Context) ([]workspace.Project, error) {
	return nil, errors.New("db down")
}

// With the workspaces the page carries the start counter and the projects; a part that cannot be read is left out and reported, the fleet stays.
func TestMetricsRouterWorkspaces(t *testing.T) {
	cfg, wg, err := fleet.ParseConfig(`{"enabled":true}`, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	cat := workspace.Config{Profiles: []workspace.Profile{{Slug: "cpu", Name: "CPU"}}}
	get := func(store workspace.Store) string {
		ws := workspace.NewService(store, cat, func(context.Context) ([]workspace.Running, error) { return nil, nil })
		_ = ws.CheckStart(context.Background(), nil, workspace.StartRequest{Person: "a@dhbw.de", Profile: "cpu"})
		h := MetricsRouter(MetricsOptions{Fleet: fleet.NewService(cfg, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir())), Workspaces: ws, Log: zap.NewNop().Sugar(), DevMode: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	out := get(workspace.NewMemoryStore())
	for _, want := range []string{"aiaas_fleet_enabled 1\n", `aiaas_workspace_starts_total{profile="cpu",result="project"} 1` + "\n", `aiaas_workspace_projects{state="active"} 0` + "\n", `aiaas_metrics_part_up{part="workspaces"} 1` + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	out = get(failingProjects{})
	if !strings.Contains(out, `aiaas_metrics_part_up{part="workspaces"} 0`+"\n") || strings.Contains(out, "aiaas_workspace_projects{") || !strings.Contains(out, "aiaas_fleet_enabled 1\n") || !strings.Contains(out, "aiaas_workspace_starts_total{") {
		t.Fatalf("failing part:\n%s", out)
	}
}
