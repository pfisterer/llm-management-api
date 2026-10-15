package gpu

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

// Model weights stay in a node's cache (/var/lib/dhbw-gpu/cache) after its gap filler moved on to another model, 30–40 GB each on the large tier. The janitor (DaemonSet model-cache-janitor, Ansible tasks/inference.yaml) removes from every GPU node's cache what this ConfigMap does not keep for the node and nobody read for a while; the list says what the node needs now.
const modelCacheKeepName = "model-cache-keep"

// modelCacheKeep is per GPU node the Hugging Face repositories its cache keeps, one per line: the model of its gap filler and what any vLLM pod on it runs (the base load, a gap filler still on its old model). Every GPU node has an entry, an empty one too: the janitor leaves a node without an entry alone.
func modelCacheKeep(nodes []GPUNode, models map[string]CatalogModel, tiers map[string]string, pods []podInfo) map[string]string {
	keep := map[string]map[string]bool{}
	add := func(node, repo string) {
		if node == "" || repo == "" {
			return
		}
		if keep[node] == nil {
			keep[node] = map[string]bool{}
		}
		keep[node][repo] = true
	}
	for _, n := range nodes {
		if keep[n.Name] == nil {
			keep[n.Name] = map[string]bool{}
		}
		if m, ok := gapModelFor(n, models, tiers); ok {
			add(n.Name, m.Model)
		}
	}
	for _, p := range pods {
		add(p.Spec.NodeName, modelRepo(p))
	}
	out := make(map[string]string, len(keep))
	for node, repos := range keep {
		list := make([]string, 0, len(repos))
		for r := range repos {
			list = append(list, r)
		}
		sort.Strings(list)
		out[node] = strings.Join(list, "\n")
	}
	return out
}

// modelRepo is the repository a vLLM pod serves (--model), not the name it serves it under.
func modelRepo(p podInfo) string {
	for _, c := range p.Spec.Containers {
		for i, a := range c.Args {
			if a == "--model" && i+1 < len(c.Args) {
				return c.Args[i+1]
			}
			if v, ok := strings.CutPrefix(a, "--model="); ok {
				return v
			}
		}
	}
	return ""
}

// ensureModelCacheKeep writes the keep list into the ConfigMap, only when it changed.
func ensureModelCacheKeep(ctx context.Context, k kubeDoer, ns string, data map[string]string) error {
	path := "/api/v1/namespaces/" + ns + "/configmaps/" + modelCacheKeepName
	var cur map[string]any
	err := k.do(ctx, http.MethodGet, path, nil, &cur)
	if errors.Is(err, ErrNotFound) {
		cm := map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": modelCacheKeepName, "namespace": ns, "labels": map[string]any{managedLabel: managedValue}},
			"data":     data,
		}
		return k.do(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/configmaps", cm, nil)
	}
	if err != nil {
		return err
	}
	have := map[string]string{}
	if d, ok := cur["data"].(map[string]any); ok {
		for key, v := range d {
			s, _ := v.(string)
			have[key] = s
		}
	}
	if reflect.DeepEqual(have, data) {
		return nil
	}
	cur["data"] = data
	return k.do(ctx, http.MethodPut, path, cur, nil)
}
