// Package gpu is the GPU part of the service: environments built from Git
// repositories (repo2docker jobs in the GPU cluster, images in Harbor) and
// JupyterHub servers started from them. It talks to three APIs of the GPU
// cluster: Kubernetes (build jobs, through the WireGuard link of this node),
// JupyterHub and Harbor (both through their public hosts).
package gpu

import (
	"fmt"
	"strings"
)

// Tier is one GPU quota class. MaxGPUs is how many GPUs a person may use at
// the same time (GPU servers and GPU kernels together).
type Tier struct {
	Name    string `json:"name"`
	MaxGPUs int    `json:"maxGPUs"`
}

// Config of the GPU part. Enabled=false switches it off entirely: no routes,
// no clients, GPU tiers still accepted in access rules (empty list).
type Config struct {
	Enabled bool
	Tiers   []Tier

	KubeAPIURL     string // https://10.91.0.1:6443
	KubeToken      string // ServiceAccount token, namespace BuildNamespace
	KubeCA         string // PEM of the cluster CA
	BuildNamespace string // env-build

	Registry      string // registry.gpu.services.dhbw.cloud
	EnvsProject   string // envs
	BuilderImage  string // optional; default: newest tag of platform/repo2docker-buildkit
	BuilderSecret string // pull/push secret in BuildNamespace (harbor-envs-builder)
	GitHosts      []string

	JupyterURL      string // https://jupyter.gpu.services.dhbw.cloud
	JupyterAPIToken string // token of the hub service gpu-management-api
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	var missing []string
	for name, v := range map[string]string{"GPU_KUBE_API_URL": c.KubeAPIURL, "GPU_KUBE_TOKEN": c.KubeToken, "GPU_KUBE_CA": c.KubeCA,
		"GPU_REGISTRY": c.Registry, "JUPYTERHUB_URL": c.JupyterURL, "JUPYTERHUB_API_TOKEN": c.JupyterAPIToken} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(c.Tiers) == 0 {
		missing = append(missing, "GPU_TIERS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("GPU part enabled but missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// TierNames lists the configured tier names.
func (c Config) TierNames() []string {
	out := make([]string, 0, len(c.Tiers))
	for _, t := range c.Tiers {
		out = append(out, t.Name)
	}
	return out
}
