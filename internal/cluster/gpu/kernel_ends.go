package gpu

import (
	"time"

	"github.com/pfisterer/cloud-self-service-golib/promtext"
	"github.com/pfisterer/llm-management-api/internal/cluster/research"
)

// KernelEndReasons are the reasons a GPU kernel ends with, as aiaas_gpu_kernels_ended_total counts them (the research log's End* reasons that apply to kernels): idle_release (its server gave it back after the idle time, dhbw_release), stopped_person (the person shut it down), stopped_server (its server ended it without saying why), cancelled and launch_timeout (while waiting), server_stopped (its server ended), max_runtime (activeDeadlineSeconds), evicted_node (the kubelet, e.g. local storage over its limit), oom, failed, completed (the kernel process ended itself), preempted, deactivated, evicted_kueue, evicted_api (node drain), node_failure, deleted (cause unknown) and gone (ended while the service was down).
var KernelEndReasons = []string{research.EndIdleRelease, research.EndStoppedPerson, research.EndStoppedServer, research.EndCancelled, research.EndLaunchTimeout, research.EndServerStopped,
	research.EndMaxRuntime, research.EndEvictedNode, research.EndOOM, research.EndFailed, research.EndCompleted, research.EndPreempted, research.EndDeactivated, research.EndEvictedKueue,
	research.EndEvictedAPI, research.EndNodeFailure, research.EndDeleted, research.EndGone}

func newKernelEndCounter() *promtext.CounterVec {
	c := promtext.NewCounterVec("aiaas_gpu_kernels_ended_total", "GPU kernels that ended, by reason (idle_release, stopped_person, server_stopped, max_runtime, evicted_node …), as the research log's sampler sees them every minute. Counted since the service started.", "reason")
	for _, r := range KernelEndReasons {
		c.Init(r)
	}
	return c
}

// KernelEnded logs the end of a GPU kernel ("gpu kernel ended", the counterpart of "gpu kernel started") and counts it; other workloads are ignored. Told by the research log's sampler (SetResearch), so the end is seen within a minute and its reason is the research log's: what the service was told by the notebook server (StopKernel), the pod's phase and reason, Kueue's events, or that its server is gone. duration is from the pod's creation (the start's log line), runtime from its container's start (missing if it never ran, e.g. cancelled while waiting).
func (s *Service) KernelEnded(e research.Ended) {
	if e.Kind != research.KindGPUKernel {
		return
	}
	reason := e.EndReason
	if reason == "" {
		reason = research.EndDeleted
	}
	if s.kernelEnds != nil {
		s.kernelEnds.Inc(reason)
	}
	if s.log == nil {
		return
	}
	end := time.Now()
	if e.EndedAt != nil {
		end = *e.EndedAt
	}
	kv := []any{"pod", e.Name, "kernel_id", e.KernelID, "person", e.Person, "project", e.Project, "gpu_class", e.GPUClass, "node", e.Node, "reason", reason}
	if e.EndDetail != "" {
		kv = append(kv, "detail", e.EndDetail)
	}
	if !e.Created.IsZero() {
		kv = append(kv, "duration", end.Sub(e.Created).Round(time.Second).String())
	}
	if e.RunningAt != nil {
		kv = append(kv, "runtime", end.Sub(*e.RunningAt).Round(time.Second).String())
	}
	if e.EndEstimated {
		// Vanished between two samples: it ended at most a minute before.
		kv = append(kv, "end_estimated", true)
	}
	s.log.Infow("gpu kernel ended", kv...)
}
