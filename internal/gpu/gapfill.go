package gpu

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Inference gap fillers (docs/bauplan.md 10.13, phase B): every assigned GPU node runs a vLLM replica with the lowest priority, which gives its GPU back to any workspace or job (Kueue preempts it) and comes back when the GPU is free. The model is the tier's (InferenceConfig.TierModels) unless the node's assignment names another or "-" for none. The service keeps per replica a Deployment, a Service and an Ingress path <host>/gap-<node>; LiteLLM learns about the replicas through the fleet sites as before. The base load (priority base-load) stays with Ansible.

// InferenceModel is a model a replica can serve, with the vLLM arguments and resources that fit it to a GPU size.
type InferenceModel struct {
	Model     string         `json:"model"`
	Args      []string       `json:"args,omitempty"`
	Resources map[string]any `json:"resources,omitempty"`
	MinGB     float64        `json:"minGB,omitempty"` // GPU memory it needs
}

// InferenceConfig is what the gap fillers need: the vLLM image, the public host, the models by key and the default model key of each tier.
type InferenceConfig struct {
	Image      string                    `json:"image"`
	Hostname   string                    `json:"hostname"`
	Models     map[string]InferenceModel `json:"models"`
	TierModels map[string]string         `json:"tierModels"`
	// GapDelayMinutes: an evicted gap filler comes back only after its node's GPU was free this long (default 5, 0 = at once). Bridges a restarted server, a new kernel after the idle cull and students starting one after another, without vLLM starting and being evicted again in between.
	GapDelayMinutes *int `json:"gapDelayMinutes,omitempty"`
}

// gapDelay is GapDelayMinutes with its default.
func (c InferenceConfig) gapDelay() time.Duration {
	if c.GapDelayMinutes == nil {
		return 5 * time.Minute
	}
	return time.Duration(max(*c.GapDelayMinutes, 0)) * time.Minute
}

const (
	gapRole      = "gap-filler"
	managedLabel = "app.kubernetes.io/managed-by"
	managedValue = "llm-management-api"
	// NoGapFiller in a node's assignment switches its gap filler off.
	NoGapFiller = "-"
)

// TierModels is the default gap filler model of each tier.
func (s *Service) TierModels(ctx context.Context) map[string]string {
	_, tiers, err := s.catalogue(ctx)
	if err != nil {
		return map[string]string{}
	}
	return tiers
}

// gapModelFor is the catalogue model of a node's gap filler; false = none.
func gapModelFor(n GPUNode, models map[string]CatalogModel, tiers map[string]string) (CatalogModel, bool) {
	if n.Assignment == nil || n.Class == "" || n.Assignment.GapModel == NoGapFiller {
		return CatalogModel{}, false
	}
	key := n.Assignment.GapModel
	if key == "" {
		key = tiers[n.Assignment.Tier]
	}
	m, ok := models[key]
	return m, ok
}

type gapReplica struct {
	name, node, class, model string
}

// ReconcileGapFillers keeps one gap filler per assigned GPU node with a model, and removes the others (also those Ansible created before, vllm-gap-*). A replica whose class changed is recreated: Kueue keeps a workload's queue for its life.
func (s *Service) ReconcileGapFillers(ctx context.Context) error {
	k, ok := s.pods.(fleetKube)
	if !ok || s.fleet == nil || s.fleetCfg.Inference.Image == "" {
		return nil
	}
	ns := s.cfg.InferenceNamespace
	if ns == "" {
		ns = "inference"
	}
	nodes, err := s.GPUNodes(ctx)
	if err != nil {
		return err
	}
	models, tiers, err := s.catalogue(ctx)
	if err != nil {
		return err
	}
	want := map[string]gapReplica{}
	for _, n := range nodes {
		if m, ok := gapModelFor(n, models, tiers); ok {
			// Object names without dots: a Service may not have any, and nodes outside the cluster are named by their FQDN (dell-gb10-2.dhbw.cloud).
			r := gapReplica{name: "gap-" + strings.ReplaceAll(n.Name, ".", "-"), node: n.Name, class: n.Class, model: m.Key}
			if len("vllm-"+r.name) <= 63 {
				want["vllm-"+r.name] = r
			}
		}
	}
	var errs []error
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return err
	}
	for _, c := range classes {
		if err := ensureObject(ctx, k, inferenceQueueFor(ns, c.ID)); err != nil {
			errs = append(errs, err)
		}
	}
	// What is there: the gap fillers of this service and of Ansible before it.
	var deps struct {
		Items []map[string]any `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/apis/apps/v1/namespaces/"+ns+"/deployments?labelSelector="+url.QueryEscape("app=vllm"), nil, &deps); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, d := range deps.Items {
		meta := obj(d, "metadata")
		name := str(meta, "name")
		if !strings.HasPrefix(name, "vllm-gap-") {
			continue // the base load is Ansible's
		}
		r, keep := want[name]
		queue := str(obj(meta, "labels"), "kueue.x-k8s.io/queue-name")
		if keep && queue == "inference-"+r.class {
			continue
		}
		s.log.Infow("gap filler removed", "deployment", name, "reason", map[bool]string{true: "class changed", false: "not planned"}[keep])
		for _, p := range []string{"/apis/apps/v1/namespaces/" + ns + "/deployments/" + name, "/api/v1/namespaces/" + ns + "/services/" + name, "/apis/networking.k8s.io/v1/namespaces/" + ns + "/ingresses/" + name} {
			if err := k.do(ctx, http.MethodDelete, p, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	busy, err := s.gapBusyNodes(ctx, k)
	if err != nil {
		errs = append(errs, err)
	}
	var gapPods []podInfo
	if busy != nil {
		if gapPods, err = s.pods.listPods(ctx, ns, "app=vllm"); err != nil {
			errs = append(errs, err)
			busy = nil
		}
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		r := want[name]
		m := models[r.model]
		dep := s.gapDeployment(ns, r, m)
		if busy != nil && s.gapHeldBack(r, busy, gapPods) {
			obj(dep, "spec")["replicas"] = 0
		}
		for _, o := range []map[string]any{dep, gapService(ns, r), s.gapIngress(ns, r)} {
			if err := ensureObject(ctx, k, o); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	// What each node's model cache keeps; the janitor removes the rest.
	if pods, err := s.pods.listPods(ctx, ns, "app=vllm"); err != nil {
		errs = append(errs, err)
	} else if err := ensureModelCacheKeep(ctx, k, ns, modelCacheKeep(nodes, models, tiers, pods)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", modelCacheKeepName, err))
	}
	return errors.Join(errs...)
}

func inferenceQueueFor(ns, class string) map[string]any {
	q := localQueueFor(ns, class)
	q["metadata"].(map[string]any)["name"] = "inference-" + class
	return q
}

func gapLabels(r gapReplica) map[string]any {
	return map[string]any{"app": "vllm", "replica": r.name, managedLabel: managedValue}
}

func (s *Service) gapDeployment(ns string, r gapReplica, m CatalogModel) map[string]any {
	queue := "inference-" + r.class
	labels := gapLabels(r)
	labels["kueue.x-k8s.io/queue-name"] = queue
	labels["kueue.x-k8s.io/priority-class"] = gapRole
	podLabels := map[string]any{"app": "vllm", "replica": r.name, labelClass: r.class, "kueue.x-k8s.io/queue-name": queue, "dhbw.cloud/inference-role": gapRole, "dhbw.cloud/inference-model": r.model}
	args := []any{"--model", m.Model, "--served-model-name", m.Model, "--port", "8000"}
	for _, a := range m.Args {
		args = append(args, a)
	}
	resources := m.resources()
	probe := func(failures int) map[string]any {
		return map[string]any{"httpGet": map[string]any{"path": "/health", "port": "http"}, "periodSeconds": 10, "failureThreshold": failures}
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "vllm-" + r.name, "namespace": ns, "labels": labels},
		"spec": map[string]any{
			"replicas": 1,
			// One GPU per replica: the old pod must free it before the new one can start.
			"strategy": map[string]any{"type": "Recreate"},
			"selector": map[string]any{"matchLabels": map[string]any{"app": "vllm", "replica": r.name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec": map[string]any{
					// A gap filler belongs to its node; its class in the selector as well, so Kueue only gives it its own flavor: one borrowed from another pool of the tier (while its own GPU is busy) has other nodes and would leave it pending for good.
					"nodeSelector": map[string]any{"kubernetes.io/hostname": r.node, labelClass: r.class},
					// Without the variables Kubernetes sets for every service of the namespace: vLLM warns about each one it does not know.
					"enableServiceLinks": false,
					// Kueue adds the flavor's toleration on admission; set here too, so the scheduler does not wait out a backoff after a preemption.
					"tolerations":                   []any{gpuToleration},
					"terminationGracePeriodSeconds": 60,
					"containers": []any{map[string]any{
						"name": "vllm", "image": s.fleetCfg.Inference.Image, "args": args,
						"envFrom":        []any{map[string]any{"secretRef": map[string]any{"name": "vllm-api-key"}}},
						"env":            []any{envVar("HF_HOME", "/cache/huggingface")},
						"ports":          []any{map[string]any{"name": "http", "containerPort": 8000}},
						"readinessProbe": probe(3),
						"startupProbe":   probe(90), // model download and load on the first start
						"resources":      resources,
						"volumeMounts":   []any{map[string]any{"name": "cache", "mountPath": "/cache"}, map[string]any{"name": "shm", "mountPath": "/dev/shm"}},
					}},
					"volumes": []any{
						// Model weights stay on the node across restarts and preemptions: a gap filler that comes back starts in seconds.
						map[string]any{"name": "cache", "hostPath": map[string]any{"path": "/var/lib/dhbw-gpu/cache", "type": "DirectoryOrCreate"}},
						map[string]any{"name": "shm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "1Gi"}},
					},
				},
			},
		},
	}
}

func gapService(ns string, r gapReplica) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "vllm-" + r.name, "namespace": ns, "labels": gapLabels(r)},
		"spec": map[string]any{
			"selector": map[string]any{"app": "vllm", "replica": r.name},
			"ports":    []any{map[string]any{"name": "http", "port": 8000, "targetPort": "http"}},
		},
	}
}

// gapIngress is the replica's path on the inference host (<host>/<replica>/v1), one Ingress per replica; Traefik merges them with the base load's.
func (s *Service) gapIngress(ns string, r gapReplica) map[string]any {
	host := s.fleetCfg.Inference.Hostname
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"name": "vllm-" + r.name, "namespace": ns, "labels": gapLabels(r), "annotations": map[string]any{
			"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
			"traefik.ingress.kubernetes.io/router.tls":         "true",
			"traefik.ingress.kubernetes.io/router.middlewares": "inference-strip-replica-prefix@kubernetescrd",
		}},
		"spec": map[string]any{
			"rules": []any{map[string]any{"host": host, "http": map[string]any{"paths": []any{map[string]any{
				"path": "/" + r.name, "pathType": "Prefix",
				"backend": map[string]any{"service": map[string]any{"name": "vllm-" + r.name, "port": map[string]any{"name": "http"}}},
			}}}}},
			"tls": []any{map[string]any{"hosts": []any{host}}},
		},
	}
}

// gapBusyNodes notes, per node, when a GPU pod of people (workspace, kernel, job; not the inference pool's vLLM replicas) last ran there, and returns these times; nil when the delay is off.
func (s *Service) gapBusyNodes(ctx context.Context, k kubeDoer) (map[string]time.Time, error) {
	if s.fleetCfg.Inference.gapDelay() == 0 {
		return nil, nil
	}
	var running struct {
		Items []podInfo `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/pods?fieldSelector="+url.QueryEscape("status.phase=Running"), nil, &running); err != nil {
		return nil, err
	}
	now := time.Now()
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	if s.gapBusy == nil {
		s.gapBusy = map[string]time.Time{}
	}
	for _, p := range running.Items {
		if p.Spec.NodeName == "" || p.Metadata.Labels["app"] == "vllm" || !holdsGPU(p) {
			continue
		}
		s.gapBusy[p.Spec.NodeName] = now
	}
	return maps.Clone(s.gapBusy), nil
}

// gapHeldBack: the replica stays at zero while its node's GPU was in use within the delay, unless its pod already holds the GPU (Kueue admitted it; it gives the GPU back when needed anyway).
func (s *Service) gapHeldBack(r gapReplica, busy map[string]time.Time, pods []podInfo) bool {
	last, ok := busy[r.node]
	if !ok || time.Since(last) >= s.fleetCfg.Inference.gapDelay() {
		return false
	}
	for _, p := range pods {
		if p.Metadata.Labels["replica"] == r.name && p.Metadata.DeletionTimestamp == nil && len(p.Spec.SchedulingGates) == 0 {
			return false
		}
	}
	return true
}
