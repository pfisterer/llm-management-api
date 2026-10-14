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

// Config of the GPU part. Enabled=false switches it off entirely: no routes, no clients.
type Config struct {
	Enabled bool

	KubeAPIURL     string // https://10.91.0.1:6443
	KubeToken      string // ServiceAccount token, namespace BuildNamespace
	KubeCA         string // PEM of the cluster CA
	BuildNamespace string // env-build

	Registry      string // registry.gpu.services.dhbw.cloud
	EnvsProject   string // envs
	BuilderImage  string // optional; default: newest tag of platform/repo2docker-buildkit
	BuilderSecret string // pull/push secret in BuildNamespace (harbor-envs-builder)
	GitHosts      []string
	CourseBase    string // notebook-base at its release (registry/platform/notebook-base:v1.1.0): base of course images; empty = no course images from Git

	JupyterURL      string // https://jupyter.gpu.services.dhbw.cloud
	JupyterAPIToken string // token of the hub service gpu-management-api

	// vLLM replicas of the inference pool, reported to the discovery job as sites. Empty InferenceURL switches that off.
	InferenceURL       string // https://inference.gpu.services.dhbw.cloud; a replica answers under <url>/<replica>/v1
	InferenceAPIKey    string // the replicas' VLLM_API_KEY
	InferenceNamespace string // inference

	WorkspaceNamespace string // jupyterhub: where the workspace pods run whose usage is booked
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
	if c.InferenceURL != "" && strings.TrimSpace(c.InferenceAPIKey) == "" {
		missing = append(missing, "GPU_INFERENCE_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("GPU part enabled but missing: %s", strings.Join(missing, ", "))
	}
	return nil
}
