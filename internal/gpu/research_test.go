package gpu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/research"
)

// fakeResearchKube lists the workspace pods, a Kueue Workload per kernel and Kueue's events.
type fakeResearchKube struct {
	fakePods
	workloads, events string
}

func (f fakeResearchKube) listPods(ctx context.Context, ns, selector string) ([]podInfo, error) {
	if ns != "jupyterhub" {
		return nil, nil
	}
	return f.fakePods.listPods(ctx, ns, selector)
}

func (f fakeResearchKube) do(_ context.Context, _, path string, _, out any) error {
	switch {
	case strings.Contains(path, "/workloads"):
		return json.Unmarshal([]byte(`{"items":`+f.workloads+`}`), out)
	case strings.Contains(path, "/events") && strings.Contains(path, "Preempted"):
		return json.Unmarshal([]byte(`{"items":`+f.events+`}`), out)
	}
	return json.Unmarshal([]byte(`{"items":[]}`), out)
}

func TestResearchSnapshot(t *testing.T) {
	s, _, _, _ := testService()
	k := fakeResearchKube{fakePods: fakePods{`[
		{"metadata":{"name":"jupyter-a","uid":"s1","labels":{"component":"singleuser-server","hub.jupyter.org/servername":"ml-cpu-gpukernel","dhbw.cloud/project":"ml","dhbw.cloud/profile":"cpu-gpukernel"},"annotations":{"hub.jupyter.org/username":"a@dhbw.de"},"creationTimestamp":"2026-10-22T08:00:00Z"},
		 "spec":{"containers":[{"resources":{"requests":{"cpu":"1"}}}]},"status":{"phase":"Running"}},
		{"metadata":{"name":"kernel-1","uid":"k1","labels":{"component":"kernel","dhbw.cloud/gpu-class":"l4","dhbw.cloud/project":"ml"},"ownerReferences":[{"kind":"Pod","name":"jupyter-a","uid":"s1"}],"creationTimestamp":"2026-10-22T09:00:00Z",
		  "deletionTimestamp":"2026-10-22T10:00:30Z","deletionGracePeriodSeconds":30},
		 "spec":{"nodeName":"gpu-1","priority":0,"containers":[{"resources":{"limits":{"nvidia.com/gpu":"1"}}}]},
		 "status":{"phase":"Running","conditions":[{"type":"PodScheduled","status":"True","lastTransitionTime":"2026-10-22T09:03:00Z"},{"type":"Ready","status":"True","lastTransitionTime":"2026-10-22T09:04:10Z"}],
		  "containerStatuses":[{"state":{"running":{"startedAt":"2026-10-22T09:04:00Z"}}}]}}
	]`}, workloads: `[{"metadata":{"uid":"wl-k1","ownerReferences":[{"kind":"Pod","uid":"k1"}]},"spec":{"priorityClassName":"workspace"},
		"status":{"admission":{"podSetAssignments":[{"flavors":{"nvidia.com/gpu":"l4-borrowed"}}]},"conditions":[
		{"type":"QuotaReserved","status":"True","lastTransitionTime":"2026-10-22T09:02:00Z"},{"type":"Admitted","status":"True","lastTransitionTime":"2026-10-22T09:02:01Z"},
		{"type":"Evicted","status":"True","reason":"Preempted","message":"Preempted to accommodate a workload (UID: 11111111-2222-3333-4444-555555555555, JobUID: 21111111-2222-3333-4444-555555555555) due to x","lastTransitionTime":"2026-10-22T10:00:00Z"}]}}]`,
		events: `[{"involvedObject":{"uid":"wl-v"},"reason":"Preempted","message":"m","lastTimestamp":"2026-10-22T09:01:00Z"}]`}
	s.pods, s.k8s = k, k
	snap, err := s.ResearchSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Pods) != 2 || len(snap.Events) != 1 || snap.Events[0].UID != "wl-v" {
		t.Fatalf("snapshot: %+v", snap)
	}
	by := map[string]research.Pod{}
	for _, p := range snap.Pods {
		by[p.UID] = p
	}
	srv := by["s1"]
	if srv.Kind != research.KindCPUServer || srv.Person != "a@dhbw.de" || srv.Server != "ml-cpu-gpukernel" || srv.Project != "ml" {
		t.Fatalf("server: %+v", srv)
	}
	kp := by["k1"]
	ts := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if kp.Kind != research.KindGPUKernel || kp.Person != "a@dhbw.de" || kp.OwnerUID != "s1" || kp.GPUClass != "l4" || kp.Flavor != "l4-borrowed" || kp.GPUs != 1 || kp.Node != "gpu-1" || kp.PriorityClass != "workspace" || kp.WorkloadUID != "wl-k1" {
		t.Fatalf("kernel: %+v", kp)
	}
	if !kp.AdmittedAt.Equal(ts("2026-10-22T09:02:00Z")) || !kp.ScheduledAt.Equal(ts("2026-10-22T09:03:00Z")) || !kp.RunningAt.Equal(ts("2026-10-22T09:04:00Z")) || !kp.ReadyAt.Equal(ts("2026-10-22T09:04:10Z")) {
		t.Fatalf("kernel times: %+v", kp)
	}
	if !kp.DeletingAt.Equal(ts("2026-10-22T10:00:00Z")) || kp.KueueEvicted != "Preempted" || !strings.Contains(kp.KueueMessage, "JobUID") {
		t.Fatalf("kernel end: %+v", kp)
	}
}
