package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Harbor's artifact API with scan overviews, as Harbor 2.x answers: an image with its own scan, an index whose amd64 variant has it, one not scanned yet.
func TestHarborScan(t *testing.T) {
	const report = `"scan_overview":{"application/vnd.security.vulnerability.report; version=1.1":{"report_id":"r1","scan_status":"Success","severity":"Critical","duration":31,"summary":{"total":17,"fixable":17,"summary":{"Critical":2,"High":5,"Medium":10}},"start_time":"2026-10-17T09:59:29Z","end_time":"2026-10-17T10:00:00Z","complete_percent":100}}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("with_scan_overview") != "true" || !strings.Contains(r.Header.Get("X-Accept-Vulnerabilities"), "vulnerability.report") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v2.0/projects/envs/repositories/kurs/artifacts/sha256:one":
			fmt.Fprint(w, `{"id":7,"project_id":3,"digest":"sha256:one",`+report+`}`)
		case "/api/v2.0/projects/platform/repositories/base%252Fpython/artifacts/v2":
			fmt.Fprint(w, `{"id":8,"project_id":2,"digest":"sha256:index","references":[{"child_digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"child_digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`)
		case "/api/v2.0/projects/platform/repositories/base%252Fpython/artifacts/sha256:amd":
			fmt.Fprint(w, `{"id":9,"project_id":2,"digest":"sha256:amd",`+report+`}`)
		case "/api/v2.0/projects/envs/repositories/neu/artifacts/c-1":
			fmt.Fprint(w, `{"id":10,"project_id":3,"digest":"sha256:neu","scan_overview":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := &Client{HTTP: srv.Client()}
	ctx := context.Background()

	sc, err := c.HarborScan(ctx, host+"/envs/kurs:c-1@sha256:one", nil)
	if err != nil || sc == nil || sc.Status != "Success" || sc.Critical != 2 || sc.High != 5 || !sc.ScannedAt.Equal(time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)) || sc.URL != "https://"+host+"/harbor/projects/3/repositories/kurs/artifacts-tab/artifacts/sha256:one" {
		t.Fatalf("scan = %+v, %v", sc, err)
	}
	// An index: the scan of its amd64 variant, the link to the index.
	sc, err = c.HarborScan(ctx, host+"/platform/base/python:v2", nil)
	if err != nil || sc == nil || sc.Critical != 2 || sc.URL != "https://"+host+"/harbor/projects/2/repositories/base%2Fpython/artifacts-tab/artifacts/sha256:index" {
		t.Fatalf("index = %+v, %v", sc, err)
	}
	if sc, err := c.HarborScan(ctx, host+"/envs/neu:c-1", nil); err != nil || sc != nil {
		t.Errorf("not scanned = %+v, %v", sc, err)
	}
	if _, err := c.HarborScan(ctx, host+"/envs/weg:1", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing artifact: %v", err)
	}
	if _, err := c.HarborScan(ctx, host+"/einzeln:1", nil); err == nil {
		t.Error("image without a Harbor project accepted")
	}
}
