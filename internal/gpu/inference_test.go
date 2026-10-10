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

func TestInferencePool(t *testing.T) {
	s, _, _, _ := testService()
	if p, err := s.InferencePool(context.Background()); err != nil || p.Enabled || p.Pods == nil {
		t.Fatalf("off without URL, with an empty list: %+v %v", p, err)
	}
	s.cfg.InferenceURL = "https://inference.example"
	args := `"spec":{"nodeName":"gpu-1","containers":[{"args":["--model","Qwen/X","--served-model-name","qwen-x","--port","8000"]}]}`
	s.pods = fakePods{`[
		{"metadata":{"name":"p1","labels":{"replica":"gap-gpu-1","dhbw.cloud/gpu-class":"l4-3q","dhbw.cloud/inference-role":"gap-filler"}},` + args + `,"status":{"startTime":"2026-10-10T18:31:00Z","conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"p2","labels":{"replica":"base-l4-3q-0"},"creationTimestamp":"2026-10-10T18:00:00Z"},"spec":{"schedulingGates":[{"name":"kueue.x-k8s.io/admission"}],"containers":[{"args":["--model","Qwen/Y"]}]},"status":{}},
		{"metadata":{"name":"p3","labels":{"replica":"c-start"}},"spec":{"nodeName":"gpu-2"},"status":{"conditions":[{"type":"Ready","status":"False"}]}},
		{"metadata":{"name":"p4","labels":{"replica":"d-leaving"},"deletionTimestamp":"2026-10-10T19:00:00Z"},"spec":{"nodeName":"gpu-3"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}
	]`}
	p, err := s.InferencePool(context.Background())
	if err != nil || !p.Enabled || len(p.Pods) != 4 {
		t.Fatalf("pool: %+v %v", p, err)
	}
	want := map[string]string{"gap-gpu-1": InferenceReady, "base-l4-3q-0": InferenceWaiting, "c-start": InferenceStarting, "d-leaving": InferenceTerminating}
	for _, ip := range p.Pods {
		if ip.State != want[ip.Replica] {
			t.Errorf("%s: state %q, want %q", ip.Replica, ip.State, want[ip.Replica])
		}
	}
	by := map[string]InferencePod{}
	for _, ip := range p.Pods {
		by[ip.Replica] = ip
	}
	if p.Pods[0].Replica != "base-l4-3q-0" {
		t.Fatalf("sorted by replica: %+v", p.Pods)
	}
	if g := by["gap-gpu-1"]; g.Replica != "gap-gpu-1" || g.Model != "qwen-x" || g.Site != "gpu-gap-gpu-1" || g.Node != "gpu-1" || g.Since == nil || g.Since.Hour() != 18 {
		t.Fatalf("gap filler: %+v", g)
	}
	if b := by["base-l4-3q-0"]; b.Model != "Qwen/Y" || b.Since == nil {
		t.Fatalf("waiting base load: model from --model, since = creation: %+v", b)
	}
}
