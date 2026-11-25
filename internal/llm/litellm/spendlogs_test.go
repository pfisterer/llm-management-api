package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// The spend logs are read page by page with the master key; a LiteLLM without /spend/logs/ui is read through /spend/logs/v2.
func TestSpendLogsPagesAndFallback(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer master" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/spend/logs/ui" {
			http.NotFound(w, r)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("start_date") != "2026-11-11 00:00:00" || r.URL.Query().Get("page_size") != "100" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		n := 100
		if page == 2 {
			n = 7
		}
		var data []map[string]any
		for i := 0; i < n; i++ {
			data = append(data, map[string]any{"api_key": "h", "model_group": "code", "prompt_tokens": 10, "completion_tokens": 1, "startTime": "2026-11-23T08:00:00.123000", "request_tags": []string{"User-Agent: Positron/1"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "total": 107, "page": page, "total_pages": 2})
	}))
	defer srv.Close()
	c := New(srv.URL, "master", 5*time.Second)
	logs, err := c.SpendLogs(context.Background(), time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 107 || fmt.Sprint(paths) != "[/spend/logs/ui /spend/logs/v2 /spend/logs/v2]" {
		t.Fatalf("%d logs over %v", len(logs), paths)
	}
	if logs[0].UserAgent() != "Positron/1" {
		t.Errorf("user agent = %q", logs[0].UserAgent())
	}
	if ts, ok := SpendTime(logs[0].StartTime); !ok || ts.Hour() != 8 {
		t.Errorf("time = %v %v", ts, ok)
	}
}

func TestSpendLogUserAgentFromMetadata(t *testing.T) {
	l := SpendLog{Metadata: map[string]any{"headers": map[string]any{"User-Agent": "continue/1.0"}}}
	if l.UserAgent() != "continue/1.0" {
		t.Errorf("ua = %q", l.UserAgent())
	}
	if (SpendLog{}).UserAgent() != "" {
		t.Error("none")
	}
	for _, s := range []string{"2026-11-23T08:00:00Z", "2026-11-23 08:00:00", "2026-11-23T08:00:00.5+01:00"} {
		if _, ok := SpendTime(s); !ok {
			t.Errorf("SpendTime(%q) failed", s)
		}
	}
}
