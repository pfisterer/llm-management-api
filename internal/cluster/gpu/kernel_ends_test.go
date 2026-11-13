package gpu

import (
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/cluster/research"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A GPU kernel's end is logged with its pod, person, project, class, node, reason and durations, and counted by reason; other workloads are not.
func TestKernelEnded(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	s := newService(Config{}, NewMemoryStore(), nil, nil, noHub{}, nil, zap.New(core).Sugar())
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	running, ended := t0.Add(2*time.Minute), t0.Add(92*time.Minute)
	s.KernelEnded(research.Ended{Name: "kernel-0123abcd-aabbcc", KernelID: testKernelID, Person: "a@dhbw.de", Workload: research.Workload{Kind: research.KindGPUKernel, Project: "ml", GPUClass: "general-small", Node: "gpu-1",
		Created: t0, RunningAt: &running, EndedAt: &ended, EndReason: research.EndIdleRelease}})
	s.KernelEnded(research.Ended{Workload: research.Workload{Kind: research.KindGPUKernel, Created: t0, EndedAt: &ended, EndReason: research.EndMaxRuntime, EndDetail: "DeadlineExceeded"}})
	s.KernelEnded(research.Ended{Workload: research.Workload{Kind: research.KindGPUServer, EndReason: research.EndHubStopped}})

	entries := logs.FilterMessage("gpu kernel ended").All()
	if len(entries) != 2 {
		t.Fatalf("two kernel ends logged: %+v", logs.All())
	}
	f := entries[0].ContextMap()
	want := map[string]any{"pod": "kernel-0123abcd-aabbcc", "kernel_id": testKernelID, "person": "a@dhbw.de", "project": "ml", "gpu_class": "general-small", "node": "gpu-1", "reason": "idle_release", "duration": "1h32m0s", "runtime": "1h30m0s"}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %v, want %v", k, f[k], v)
		}
	}
	if f2 := entries[1].ContextMap(); f2["detail"] != "DeadlineExceeded" || f2["reason"] != "max_runtime" || f2["runtime"] != nil {
		t.Errorf("max runtime, never ran: %v", f2)
	}
	if s.kernelEnds.Value("idle_release") != 1 || s.kernelEnds.Value("max_runtime") != 1 || s.kernelEnds.Value("hub_stopped") != 0 {
		t.Errorf("counted by reason")
	}
}

// The research snapshot's kernel pods carry their name and kernel ID for the end log.
func TestResearchPodNames(t *testing.T) {
	var p podInfo
	p.Metadata.Name, p.Metadata.UID = "kernel-0123abcd-aabbcc", "k1"
	p.Metadata.Labels = map[string]string{"component": "kernel", "kernel_id": testKernelID}
	out := researchPod(p, nil, map[string]string{}, map[string]string{})
	if out.Name != "kernel-0123abcd-aabbcc" || out.KernelID != testKernelID || out.Kind != research.KindGPUKernel {
		t.Fatalf("pod: %+v", out)
	}
}
