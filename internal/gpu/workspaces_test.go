package gpu

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

type fakeWorkspaceKube struct {
	fakePods
	metrics string
}

func (f fakeWorkspaceKube) podMetrics(context.Context, string) ([]podMetrics, error) {
	var out []podMetrics
	return out, json.Unmarshal([]byte(f.metrics), &out)
}

func TestWorkspacePods(t *testing.T) {
	s, _, _, _ := testService()
	s.pods = fakeWorkspaceKube{fakePods: fakePods{`[
		{"metadata":{"name":"jupyter-a","labels":{"component":"singleuser-server","dhbw.cloud/profile":"cpu"},"annotations":{"hub.jupyter.org/username":"a@dhbw.de"}},
		 "spec":{"containers":[{"resources":{"requests":{"cpu":"500m","memory":"2G"}}}]},"status":{"phase":"Running"}},
		{"metadata":{"name":"jupyter-b","labels":{"component":"singleuser-server","dhbw.cloud/profile":"pytorch"},"annotations":{"hub.jupyter.org/username":"b@dhbw.de"}},
		 "spec":{"nodeSelector":{"dhbw.cloud/gpu-class":"l4-3q"},"containers":[{"resources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"nvidia.com/gpu":"1"}}}]},"status":{"phase":"Running"}},
		{"metadata":{"name":"kernel-1","labels":{"component":"kernel"},"ownerReferences":[{"kind":"Pod","name":"jupyter-a"}]},
		 "spec":{"nodeSelector":{"dhbw.cloud/gpu-class":"l4-3q"},"containers":[{"resources":{"requests":{"cpu":"1","memory":"3Gi","nvidia.com/gpu":"1"}}}]},"status":{"phase":"Running"}},
		{"metadata":{"name":"jupyter-c","labels":{"component":"singleuser-server"},"annotations":{"hub.jupyter.org/username":"c@dhbw.de"}},"spec":{},"status":{"phase":"Pending"}},
		{"metadata":{"name":"orphan","labels":{"component":"kernel"}},"spec":{},"status":{"phase":"Running"}}
	]`}, metrics: `[{"metadata":{"name":"jupyter-a"},"containers":[{"usage":{"cpu":"250000000n","memory":"1048576Ki"}}]}]`}

	pods, err := s.WorkspacePods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]WorkspacePod{}
	for _, p := range pods {
		by[p.Pod] = p
	}
	if len(pods) != 4 || by["jupyter-c"].Running || !by["jupyter-a"].Running {
		t.Fatalf("running and waiting pods with a person: %+v", pods)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	if a := by["jupyter-a"]; a.Person != "a@dhbw.de" || a.Profile != "cpu" || !near(a.CPU, 0.5) || !near(a.RAMGB, 2e9/(1<<30)) || a.GPUClass != "" || !near(a.CPUUsed, 0.25) || !near(a.RAMUsedGB, 1) {
		t.Fatalf("cpu server: %+v", a)
	}
	if b := by["jupyter-b"]; b.GPUClass != "l4-3q" || !near(b.GPUs, 1) || !near(b.RAMGB, 2) {
		t.Fatalf("gpu server: %+v", b)
	}
	if k := by["kernel-1"]; k.Person != "a@dhbw.de" || k.Profile != "gpu-kernel" || k.GPUClass != "l4-3q" || !near(k.GPUs, 1) {
		t.Fatalf("kernel booked to the server's person: %+v", k)
	}
}

func TestQuantities(t *testing.T) {
	for q, want := range map[string]float64{"2": 2, "500m": 0.5, "1234567n": 0.001234567, "": 0, "x": 0} {
		if got := parseCPU(q); math.Abs(got-want) > 1e-12 {
			t.Errorf("cpu %q = %v, want %v", q, got, want)
		}
	}
	for q, want := range map[string]float64{"1Gi": 1, "512Mi": 0.5, "1048576Ki": 1, "1073741824": 1, "2G": 2e9 / (1 << 30)} {
		if got := parseGiB(q); math.Abs(got-want) > 1e-12 {
			t.Errorf("mem %q = %v, want %v", q, got, want)
		}
	}
}
