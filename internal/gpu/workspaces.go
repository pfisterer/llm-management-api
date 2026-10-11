package gpu

import (
	"context"
	"strconv"
	"strings"
)

// WorkspacePod is one running workspace pod (Jupyter server or GPU kernel) with what it reserves and, when metrics-server answers, what it uses.
type WorkspacePod struct {
	Pod       string
	Running   bool   // false: waiting (Kueue, image pull); reserves nothing yet, but counts for the limits on concurrent use
	Project   string // label dhbw.cloud/project set by the hub; empty for pods started before projects
	Person    string // JupyterHub user name (e-mail)
	Profile   string // label dhbw.cloud/profile set by the hub; "gpu-kernel" for kernel pods
	GPUClass  string // from the node selector Kueue set; empty without GPU
	CPU       float64
	RAMGB     float64
	GPUs      float64
	CPUUsed   float64
	RAMUsedGB float64
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
		w := WorkspacePod{Pod: p.Metadata.Name, Running: p.Status.Phase == "Running", Person: personOf[p.Metadata.Name],
			Project: p.Metadata.Labels["dhbw.cloud/project"], Profile: p.Metadata.Labels["dhbw.cloud/profile"]}
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
			w.GPUClass = p.Spec.NodeSelector["dhbw.cloud/gpu-class"]
			if w.GPUClass == "" {
				w.GPUClass = "unbekannt"
			}
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
