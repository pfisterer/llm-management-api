package gpu

import (
	"context"
	"encoding/json"
	"testing"
)

type fakePods struct{ items string }

func (f fakePods) listPods(context.Context, string, string) ([]podInfo, error) {
	var out []podInfo
	return out, json.Unmarshal([]byte(f.items), &out)
}

func TestInferenceReplicas(t *testing.T) {
	s, _, _, _ := testService()
	if s.InferenceEnabled() {
		t.Fatal("must be off without URL")
	}
	s.cfg.InferenceURL = "https://inference.example/"
	s.pods = fakePods{`[
		{"metadata":{"name":"a","labels":{"replica":"gap-b","dhbw.cloud/gpu-class":"l4-3q","dhbw.cloud/inference-role":"gap-filler"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"b","labels":{"replica":"gap-a"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"c","labels":{"replica":"gated"}},"status":{"conditions":[{"type":"PodScheduled","status":"False"}]}},
		{"metadata":{"name":"d","labels":{"replica":"starting"}},"status":{"conditions":[{"type":"Ready","status":"False"}]}},
		{"metadata":{"name":"e","labels":{"replica":"leaving"},"deletionTimestamp":"2026-10-10T10:00:00Z"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"f","labels":{}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
	]`}
	if !s.InferenceEnabled() {
		t.Fatal("must be on with URL and pods")
	}
	reps, err := s.InferenceReplicas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 2 || reps[0].Name != "gap-a" || reps[1].Name != "gap-b" {
		t.Fatalf("only ready, not terminating, labelled replicas, sorted: %+v", reps)
	}
	if r := reps[1]; r.APIBase != "https://inference.example/gap-b/v1" || r.GPUClass != "l4-3q" || r.Role != "gap-filler" {
		t.Fatalf("replica: %+v", r)
	}
}
