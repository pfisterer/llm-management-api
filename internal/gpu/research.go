package gpu

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/pfisterer/llm-management-api/internal/research"
)

// SetResearch sets the research log (internal/research): it is told why the service itself ends a workspace pod and what JupyterLab shows a waiting GPU kernel. nil = none.
func (s *Service) SetResearch(r *research.Recorder) { s.research = r }

// kueueWorkload is the part of a Kueue Workload the research log reads.
type kueueWorkload struct {
	Metadata struct {
		UID             string `json:"uid"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			UID  string `json:"uid"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		PriorityClassName string `json:"priorityClassName"`
		PriorityClassRef  *struct {
			Name string `json:"name"`
		} `json:"priorityClassRef"`
	} `json:"spec"`
	Status struct {
		Admission *struct {
			PodSetAssignments []struct {
				Flavors map[string]string `json:"flavors"`
			} `json:"podSetAssignments"`
		} `json:"admission"`
		Conditions []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Reason             string    `json:"reason"`
			Message            string    `json:"message"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// kubeEvent is the part of a Kubernetes event the research log reads.
type kubeEvent struct {
	InvolvedObject struct {
		UID string `json:"uid"`
	} `json:"involvedObject"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	EventTime      *time.Time `json:"eventTime"`
	FirstTimestamp *time.Time `json:"firstTimestamp"`
	LastTimestamp  *time.Time `json:"lastTimestamp"`
}

// researchEventReasons are the events that tell why Kueue stopped a pod: "Stopped" on the pod, "Preempted" on its Workload.
var researchEventReasons = []string{"Stopped", "Preempted"}

// ResearchSnapshot lists for the research log the workspace pods (servers, GPU kernels), the vLLM replicas of the inference pool and the image builds, with their Kueue Workloads and Kueue's stop events. The pods must be readable, else nothing is ended; Workloads and events are best effort (without them admission times and Kueue's reasons are missing).
func (s *Service) ResearchSnapshot(ctx context.Context) (research.Snapshot, error) {
	if s.pods == nil || s.k8s == nil {
		return research.Snapshot{}, errors.New("no GPU cluster")
	}
	type source struct {
		ns, selector string
		kueue        bool
	}
	sources := []source{{s.kernelNamespace(), "component in (singleuser-server,kernel)", true}}
	if s.InferenceEnabled() {
		ns := s.cfg.InferenceNamespace
		if ns == "" {
			ns = "inference"
		}
		sources = append(sources, source{ns, "app=vllm", true})
	}
	sources = append(sources, source{s.cfg.BuildNamespace, "job-name", false})

	productOf := map[string]string{}
	if nl, ok := s.pods.(nodeLister); ok {
		if nodes, err := nl.listNodes(ctx, "dhbw.cloud/node-type=gpu"); err == nil {
			for _, n := range nodes {
				productOf[n.Metadata.Name] = n.Metadata.Labels["nvidia.com/gpu.product"]
			}
		}
	}
	var snap research.Snapshot
	var errs []error
	for _, src := range sources {
		pods, err := s.pods.listPods(ctx, src.ns, src.selector)
		if err != nil {
			return research.Snapshot{}, err
		}
		workloadOf := map[string]kueueWorkload{}
		if src.kueue {
			var wls struct {
				Items []kueueWorkload `json:"items"`
			}
			if err := s.k8s.do(ctx, http.MethodGet, "/apis/kueue.x-k8s.io/v1beta2/namespaces/"+src.ns+"/workloads", nil, &wls); err != nil {
				errs = append(errs, err)
			}
			for _, w := range wls.Items {
				for _, o := range w.Metadata.OwnerReferences {
					if o.Kind == "Pod" {
						workloadOf[o.UID] = w
					}
				}
			}
			for _, reason := range researchEventReasons {
				var evs struct {
					Items []kubeEvent `json:"items"`
				}
				if err := s.k8s.do(ctx, http.MethodGet, "/api/v1/namespaces/"+src.ns+"/events?fieldSelector="+url.QueryEscape("reason="+reason), nil, &evs); err != nil {
					errs = append(errs, err)
					continue
				}
				for _, e := range evs.Items {
					at := e.EventTime
					if e.LastTimestamp != nil {
						at = e.LastTimestamp
					} else if at == nil {
						at = e.FirstTimestamp
					}
					if at != nil && e.InvolvedObject.UID != "" {
						snap.Events = append(snap.Events, research.Event{UID: e.InvolvedObject.UID, Reason: e.Reason, Message: e.Message, At: *at})
					}
				}
			}
		}
		// Kernel pods carry no hub annotation; their person is their server pod's.
		personOf := map[string]string{}
		for _, p := range pods {
			if u := p.Metadata.Annotations["hub.jupyter.org/username"]; u != "" {
				personOf[p.Metadata.UID] = u
			}
		}
		for _, p := range pods {
			if p.Metadata.UID == "" {
				continue
			}
			var w *kueueWorkload
			if wl, ok := workloadOf[p.Metadata.UID]; ok {
				w = &wl
			}
			snap.Pods = append(snap.Pods, researchPod(p, w, personOf, productOf))
		}
	}
	if len(errs) > 0 && s.log != nil {
		s.log.Warnw("research log: Kueue workloads or events not readable", "error", errors.Join(errs...))
	}
	return snap, nil
}

// researchPod turns a pod (and its Kueue Workload, if any) into what the research log keeps.
func researchPod(p podInfo, w *kueueWorkload, personOf, productOf map[string]string) research.Pod {
	labels := p.Metadata.Labels
	out := research.Pod{UID: p.Metadata.UID, Project: labels["dhbw.cloud/project"], Profile: labels["dhbw.cloud/profile"], Created: p.Metadata.CreationTimestamp,
		Node: p.Spec.NodeName, Phase: p.Status.Phase, StatusReason: p.Status.Reason, PriorityClass: labels["kueue.x-k8s.io/priority-class"]}
	if p.Spec.PriorityClassName != "" || p.Spec.Priority != 0 {
		prio := p.Spec.Priority
		out.Priority = &prio
	}
	switch {
	case labels["component"] == "kernel":
		out.Kind, out.Profile = research.KindGPUKernel, "gpu-kernel"
		for _, o := range p.Metadata.OwnerReferences {
			if o.Kind == "Pod" {
				out.OwnerUID, out.Person = o.UID, personOf[o.UID]
			}
		}
	case labels["component"] == "singleuser-server":
		out.Kind, out.Person, out.Server = research.KindCPUServer, p.Metadata.Annotations["hub.jupyter.org/username"], labels["hub.jupyter.org/servername"]
	case labels["app"] == "vllm":
		out.Kind, out.Model, out.Profile = research.KindVLLM, servedModel(p), labels["dhbw.cloud/inference-role"]
	default:
		out.Kind = research.KindBuild
	}
	for _, c := range p.Spec.Containers {
		g := c.Resources.Limits["nvidia.com/gpu"]
		if g == "" {
			g = c.Resources.Requests["nvidia.com/gpu"]
		}
		out.GPUs += parseCPU(g)
	}
	if out.GPUs > 0 {
		if out.Kind == research.KindCPUServer {
			out.Kind = research.KindGPUServer
		}
		out.GPUClass = labels[labelClass]
		if out.GPUClass == "" {
			out.GPUClass = p.Spec.NodeSelector[labelClass]
		}
		out.GPUProduct = productOf[p.Spec.NodeName]
	}
	for _, c := range p.Status.Conditions {
		if c.Status != "True" || c.LastTransitionTime.IsZero() {
			continue
		}
		at := c.LastTransitionTime
		switch c.Type {
		case "PodScheduled":
			out.ScheduledAt = &at
		case "Ready":
			out.ReadyAt = &at
		case "DisruptionTarget":
			out.Disruption = c.Reason
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		started := time.Time{}
		if r := c.State.Running; r != nil {
			started = r.StartedAt
		}
		if t := c.State.Terminated; t != nil {
			started = t.StartedAt
			if out.FinishedAt == nil || t.FinishedAt.After(*out.FinishedAt) {
				end := t.FinishedAt
				out.FinishedAt = &end
			}
			if t.Reason != "" && t.Reason != "Completed" && out.ContainerReason == "" {
				out.ContainerReason = t.Reason
			}
		}
		if !started.IsZero() && (out.RunningAt == nil || started.Before(*out.RunningAt)) {
			out.RunningAt = &started
		}
	}
	if out.FinishedAt != nil && out.FinishedAt.IsZero() {
		out.FinishedAt = nil
	}
	if d := p.Metadata.DeletionTimestamp; d != nil {
		at := d.Add(-time.Duration(p.Metadata.DeletionGracePeriodSeconds) * time.Second)
		out.DeletingAt = &at
	}
	if w != nil {
		out.WorkloadUID = w.Metadata.UID
		if pc := w.Spec.PriorityClassName; pc != "" {
			out.PriorityClass = pc
		} else if ref := w.Spec.PriorityClassRef; ref != nil && ref.Name != "" {
			out.PriorityClass = ref.Name
		}
		if a := w.Status.Admission; a != nil {
			for _, ps := range a.PodSetAssignments {
				if f := ps.Flavors["nvidia.com/gpu"]; f != "" {
					out.Flavor = f
				}
			}
		}
		var reserved, admitted *time.Time
		for _, c := range w.Status.Conditions {
			if c.Status != "True" {
				continue
			}
			at := c.LastTransitionTime
			switch c.Type {
			case "QuotaReserved":
				reserved = &at
			case "Admitted":
				admitted = &at
			case "Evicted", "Preempted":
				if out.KueueEvicted == "" || c.Type == "Evicted" {
					out.KueueEvicted, out.KueueMessage, out.KueueEvictedAt = c.Reason, c.Message, &at
					if c.Type == "Preempted" {
						out.KueueEvicted = "Preempted"
					}
				}
			}
		}
		// Admitted is QuotaReserved plus the admission checks (none here, so the same moment); the quota decision is the earlier one.
		out.AdmittedAt = reserved
		if out.AdmittedAt == nil {
			out.AdmittedAt = admitted
		}
	}
	if out.PriorityClass == "" {
		out.PriorityClass = p.Spec.PriorityClassName
	}
	return out
}
