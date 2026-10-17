package gpu

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// WorkspacePod is one running workspace pod (Jupyter server or GPU kernel) with what it reserves and, when metrics-server answers, what it uses.
type WorkspacePod struct {
	Pod      string
	Running  bool   // false: waiting (Kueue, image pull); reserves nothing yet, but counts for the limits on concurrent use
	Queued   bool   // still waiting for its place: gated by Kueue or on no node yet (a pod pulling its image on the GPU node holds the GPU already)
	Server   string // JupyterHub's server name (label hub.jupyter.org/servername); "" = the default server
	Project  string // label dhbw.cloud/project set by the hub; empty for pods started before projects
	Person   string // JupyterHub user name (e-mail)
	Profile  string // label dhbw.cloud/profile set by the hub; "gpu-kernel" for kernel pods
	GPUClass string // from the node selector Kueue set; empty without GPU
	// GPUProduct is the GPU type of the pod's node (nvidia.com/gpu.product); empty without GPU or before the pod has a node.
	GPUProduct string
	CPU        float64
	RAMGB      float64
	GPUs       float64
	CPUUsed    float64
	RAMUsedGB  float64
	// Created is when the pod was made (the hub's start, or the kernel's); ReadyAt when it became ready, nil while it is not: the time a start took (metrics.go).
	Created time.Time
	ReadyAt *time.Time
}

// podLister and metrics are the parts of the Kubernetes client the workspace listing needs.
type workspaceKube interface {
	podLister
	podMetrics(ctx context.Context, ns string) ([]podMetrics, error)
}

// WorkspacePods lists the workspace pods in the hub's namespace that are running or waiting; terminating and finished pods are left out. Measured use is best effort: without metrics-server the numbers stay 0.
func (s *Service) WorkspacePods(ctx context.Context) ([]WorkspacePod, error) {
	k, ok := s.pods.(workspaceKube)
	if !ok {
		return nil, nil
	}
	ns := s.cfg.WorkspaceNamespace
	if ns == "" {
		ns = "jupyterhub"
	}
	pods, err := k.listPods(ctx, ns, "component in (singleuser-server,kernel)")
	if err != nil {
		return nil, err
	}
	used := map[string][2]float64{}
	if ms, err := k.podMetrics(ctx, ns); err == nil {
		for _, m := range ms {
			var cpu, ram float64
			for _, c := range m.Containers {
				cpu += parseCPU(c.Usage["cpu"])
				ram += parseGiB(c.Usage["memory"])
			}
			used[m.Metadata.Name] = [2]float64{cpu, ram}
		}
	}
	// The GPU type of each GPU node, for the booking of what was really used within a class.
	productOf := map[string]string{}
	if nl, ok := s.pods.(nodeLister); ok {
		if nodes, err := nl.listNodes(ctx, "dhbw.cloud/node-type=gpu"); err == nil {
			for _, n := range nodes {
				productOf[n.Metadata.Name] = n.Metadata.Labels["nvidia.com/gpu.product"]
			}
		}
	}
	// Kernel pods carry no hub annotation; their owner is the person's server pod.
	personOf, projectOf := map[string]string{}, map[string]string{}
	for _, p := range pods {
		if u := p.Metadata.Annotations["hub.jupyter.org/username"]; u != "" {
			personOf[p.Metadata.Name] = u
			projectOf[p.Metadata.Name] = p.Metadata.Labels["dhbw.cloud/project"]
		}
	}
	out := []WorkspacePod{}
	for _, p := range pods {
		if (p.Status.Phase != "Running" && p.Status.Phase != "Pending") || p.Metadata.DeletionTimestamp != nil {
			continue
		}
		w := WorkspacePod{Pod: p.Metadata.Name, Running: p.Status.Phase == "Running", Queued: len(p.Spec.SchedulingGates) > 0 || p.Spec.NodeName == "", Server: p.Metadata.Labels["hub.jupyter.org/servername"], Person: personOf[p.Metadata.Name],
			Project: p.Metadata.Labels["dhbw.cloud/project"], Profile: p.Metadata.Labels["dhbw.cloud/profile"], Created: p.Metadata.CreationTimestamp}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" && !c.LastTransitionTime.IsZero() {
				at := c.LastTransitionTime
				w.ReadyAt = &at
			}
		}
		if p.Metadata.Labels["component"] == "kernel" {
			w.Profile = "gpu-kernel"
			for _, o := range p.Metadata.OwnerReferences {
				if o.Kind == "Pod" && personOf[o.Name] != "" {
					w.Person = personOf[o.Name]
					w.Project = projectOf[o.Name]
				}
			}
		}
		if w.Person == "" {
			continue
		}
		if w.Profile == "" {
			w.Profile = "default"
		}
		for _, c := range p.Spec.Containers {
			w.CPU += parseCPU(c.Resources.Requests["cpu"])
			w.RAMGB += parseGiB(c.Resources.Requests["memory"])
			g := c.Resources.Limits["nvidia.com/gpu"]
			if g == "" {
				g = c.Resources.Requests["nvidia.com/gpu"]
			}
			w.GPUs += parseCPU(g)
		}
		if w.GPUs > 0 {
			// The class chosen at the start (label set by the hub or, for kernels, this service): a GPU borrowed from another pool of the tier counts against the chosen class. Else the class of the node Kueue put it on.
			w.GPUClass = p.Metadata.Labels[labelClass]
			if w.GPUClass == "" {
				w.GPUClass = p.Spec.NodeSelector[labelClass]
			}
			if w.GPUClass == "" {
				w.GPUClass = "unbekannt"
			}
			w.GPUProduct = productOf[p.Spec.NodeName]
		}
		if u, ok := used[p.Metadata.Name]; ok {
			w.CPUUsed, w.RAMUsedGB = u[0], u[1]
		}
		out = append(out, w)
	}
	return out, nil
}

// parseCPU reads a Kubernetes CPU quantity ("2", "500m", "1234567n") as cores; also plain counts like GPUs.
func parseCPU(q string) float64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	div := 1.0
	switch q[len(q)-1] {
	case 'n':
		div, q = 1e9, q[:len(q)-1]
	case 'u':
		div, q = 1e6, q[:len(q)-1]
	case 'm':
		div, q = 1e3, q[:len(q)-1]
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0
	}
	return v / div
}

// parseGiB reads a Kubernetes memory quantity ("2G", "5Gi", "512Mi", "123456Ki", "1073741824") as GiB.
func parseGiB(q string) float64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	units := []struct {
		suffix string
		bytes  float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			mult, q = u.bytes, strings.TrimSuffix(q, u.suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0
	}
	return v * mult / (1 << 30)
}
