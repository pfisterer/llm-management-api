package gpu

import (
	"context"
	"strings"
	"testing"
)

func TestWorkspaceMetrics(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := testService()
	// One build per state, and one that belongs to no project (not counted).
	for _, e := range []Environment{{Owner: "project:a", Status: StatusReady}, {Owner: "project:b", Status: StatusBuilding}, {Owner: "project:c", Status: StatusFailed}, {Owner: "someone", Status: StatusFailed}} {
		if _, err := s.store.Save(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	s.prepulled.Store("amd64", 3)
	// At the first listing jupyter-a is ready already (its start happened before) and jupyter-c waits for its place.
	first := `[
		{"metadata":{"name":"jupyter-a","creationTimestamp":"2026-10-17T10:00:00Z","labels":{"component":"singleuser-server","dhbw.cloud/profile":"cpu"},"annotations":{"hub.jupyter.org/username":"a@dhbw.de"}},
		 "spec":{"nodeName":"cpu-1","containers":[{"resources":{"requests":{"cpu":"1"}}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-17T10:00:20Z"}]}},
		{"metadata":{"name":"jupyter-c","creationTimestamp":"2026-10-17T10:01:00Z","labels":{"component":"singleuser-server","dhbw.cloud/profile":"gpu","dhbw.cloud/gpu-class":"l4-3q"},"annotations":{"hub.jupyter.org/username":"c@dhbw.de"}},
		 "spec":{"schedulingGates":[{"name":"kueue.x-k8s.io/admission"}],"containers":[{"resources":{"limits":{"nvidia.com/gpu":"1"}}}]},"status":{"phase":"Pending"}},
		{"metadata":{"name":"jupyter-x","creationTimestamp":"2026-10-17T10:01:00Z","labels":{"component":"singleuser-server","dhbw.cloud/profile":"made-up"},"annotations":{"hub.jupyter.org/username":"x@dhbw.de"}},
		 "spec":{"nodeName":"cpu-1"},"status":{"phase":"Pending"}}
	]`
	kube := &fakeWorkspaceKube{fakePods: fakePods{first}, metrics: `[]`}
	s.pods = kube
	m := NewWorkspaceMetrics(s, []string{"gpu-kernel", "default", "cpu", "gpu"})
	raw, err := m.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		`aiaas_workspace_servers{profile="cpu",gpu_class="",state="running"} 1` + "\n",
		`aiaas_workspace_servers{profile="gpu",gpu_class="l4-3q",state="queued"} 1` + "\n",
		`aiaas_workspace_servers{profile="other",gpu_class="",state="starting"} 1` + "\n",
		`aiaas_workspace_servers{profile="gpu-kernel",gpu_class="",state="running"} 0` + "\n",
		`aiaas_workspace_gpus{gpu_class="l4-3q",state="queued"} 1` + "\n",
		`aiaas_env_builds{status="building"} 1` + "\n",
		`aiaas_env_builds{status="failed"} 1` + "\n",
		`aiaas_env_builds{status="ready"} 1` + "\n",
		`aiaas_prepull_images{arch="amd64"} 3` + "\n",
		"# TYPE aiaas_workspace_start_duration_seconds histogram\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "aiaas_workspace_start_duration_seconds_count") || strings.Contains(out, "made-up") || strings.Contains(out, "@dhbw.de") || strings.Contains(out, `arch="arm64"`) {
		t.Fatalf("first listing observes nothing, no unknown profile, no person, no arch without prepull:\n%s", out)
	}

	// Next listing: jupyter-c got its GPU and became ready after 150 s; jupyter-a went unready and ready again (not a new start).
	kube.items = `[
		{"metadata":{"name":"jupyter-a","creationTimestamp":"2026-10-17T10:00:00Z","labels":{"component":"singleuser-server","dhbw.cloud/profile":"cpu"},"annotations":{"hub.jupyter.org/username":"a@dhbw.de"}},
		 "spec":{"nodeName":"cpu-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-17T10:30:00Z"}]}},
		{"metadata":{"name":"jupyter-c","creationTimestamp":"2026-10-17T10:01:00Z","labels":{"component":"singleuser-server","dhbw.cloud/profile":"gpu","dhbw.cloud/gpu-class":"l4-3q"},"annotations":{"hub.jupyter.org/username":"c@dhbw.de"}},
		 "spec":{"nodeName":"gpu-1","containers":[{"resources":{"limits":{"nvidia.com/gpu":"1"}}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-17T10:03:30Z"}]}}
	]`
	raw, err = m.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out = string(raw)
	for _, want := range []string{
		`aiaas_workspace_start_duration_seconds_bucket{profile="gpu",le="120"} 0` + "\n",
		`aiaas_workspace_start_duration_seconds_bucket{profile="gpu",le="180"} 1` + "\n",
		`aiaas_workspace_start_duration_seconds_bucket{profile="gpu",le="+Inf"} 1` + "\n",
		`aiaas_workspace_start_duration_seconds_sum{profile="gpu"} 150` + "\n",
		`aiaas_workspace_start_duration_seconds_count{profile="gpu"} 1` + "\n",
		`aiaas_workspace_gpus{gpu_class="l4-3q",state="running"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, `start_duration_seconds_count{profile="cpu"}`) {
		t.Fatalf("a pod ready again is no new start:\n%s", out)
	}
	// The same listing again observes nothing new.
	raw, _ = m.Render(ctx)
	if !strings.Contains(string(raw), `aiaas_workspace_start_duration_seconds_count{profile="gpu"} 1`+"\n") {
		t.Fatalf("observed twice:\n%s", raw)
	}
}
