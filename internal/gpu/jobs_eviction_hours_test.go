package gpu

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// An evicted job's GPU hours end at its eviction, not at the sync that sees it wait again: on staging a job evicted 48 s before the next sync was billed those 48 s.
func TestJobHoursEndAtEviction(t *testing.T) {
	seen := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	evicted := seen.Add(5 * time.Minute)
	now := evicted.Add(48 * time.Second)

	var w jobWorkload
	if err := json.Unmarshal([]byte(`{"status":{"conditions":[{"type":"Evicted","status":"True","lastTransitionTime":"`+evicted.Format(time.RFC3339)+`"}],"schedulingStats":{"evictions":[{"count":1}]}}}`), &w); err != nil {
		t.Fatal(err)
	}
	r := JobRecord{GPUs: 1, State: JobRunning, SeenRunning: &seen}
	(&Service{}).syncJob(t.Context(), &r, k8sJob{}, w, now)

	if r.State != JobQueued || r.Evictions != 1 {
		t.Fatalf("state %s, evictions %d", r.State, r.Evictions)
	}
	if want := (5 * time.Minute).Hours(); math.Abs(r.GPUHours-want) > 1e-9 {
		t.Errorf("gpu hours %v, want %v (up to the eviction)", r.GPUHours, want)
	}
}
