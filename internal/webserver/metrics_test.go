package webserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/fleet"
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
		h := MetricsRouter(fleet.NewService(cfg, wg, store, fleet.NewScripts(t.TempDir())), zap.NewNop().Sugar(), true)
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
