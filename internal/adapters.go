package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"github.com/pfisterer/llm-management-api/internal/llm/usageprofiles"
)

// The GPU part (package gpu) knows the cluster, the workspace part (package workspace) the projects, and usage the booking; neither imports the other's view of a pod. The mappings between them live here, in one place, rather than inline in run().

func workspaceSource(g *gpu.Service) usage.Source {
	return func(ctx context.Context) ([]usage.Sample, error) {
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]usage.Sample, 0, len(pods))
		for _, p := range pods {
			if !p.Running {
				continue // waiting pods reserve nothing yet
			}
			out = append(out, usage.Sample{Project: p.Project, Person: p.Person, Profile: p.Profile, GPUClass: p.GPUClass, GPUProduct: p.GPUProduct, CPU: p.CPU, RAMGB: p.RAMGB,
				GPUs: p.GPUs, CPUUsed: p.CPUUsed, RAMUsedGB: p.RAMUsedGB})
		}
		return out, nil
	}
}

// gpuSource samples the GPUs: capacity per class from the nodes, the workspace pods with or waiting for a GPU, and the inference pool. The pool is best effort (no inference configured = none).
func gpuSource(g *gpu.Service) usage.GPUSource {
	return func(ctx context.Context) (usage.GPUSample, error) {
		capacity, err := g.GPUCapacity(ctx)
		if err != nil {
			return usage.GPUSample{}, err
		}
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return usage.GPUSample{}, err
		}
		s := usage.GPUSample{Capacity: capacity}
		for _, p := range pods {
			if p.GPUs > 0 {
				s.Pods = append(s.Pods, usage.GPUPod{GPUClass: p.GPUClass, Project: p.Project, Person: p.Person, Profile: p.Profile, GPUs: p.GPUs, Waiting: p.Queued})
			}
		}
		if inf, err := g.InferenceGPUs(ctx); err == nil {
			s.Inference = inf
		}
		return s, nil
	}
}

func runningWorkspaces(g *gpu.Service) func(context.Context) ([]workspace.Running, error) {
	return func(ctx context.Context) ([]workspace.Running, error) {
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]workspace.Running, 0, len(pods))
		for _, p := range pods {
			out = append(out, workspace.Running{Server: p.Pod, Person: p.Person, Project: p.Project, Profile: p.Profile, GPUClass: p.GPUClass,
				CPU: p.CPU, RAMGB: p.RAMGB, GPUs: p.GPUs})
		}
		return out, nil
	}
}

// runningServers are the running workspace pods as Enforce sees them.
func runningServers(pods []gpu.WorkspacePod) []workspace.RunningServer {
	var running []workspace.RunningServer
	for _, p := range pods {
		if p.Running {
			running = append(running, workspace.RunningServer{Running: workspace.Running{Server: p.Pod, Person: p.Person, Project: p.Project, Profile: p.Profile}, ServerName: p.Server})
		}
	}
	return running
}

// workspaceGPUClasses are the GPU administration's classes as the projects see them.
func workspaceGPUClasses(g *gpu.Service) func(ctx context.Context) ([]workspace.GPUClass, error) {
	return func(ctx context.Context) ([]workspace.GPUClass, error) {
		classes, err := g.GPUClasses(ctx)
		out := make([]workspace.GPUClass, 0, len(classes))
		for _, c := range classes {
			out = append(out, workspace.GPUClass{ID: c.ID, Name: c.Name, Pool: c.Pool, PoolName: c.PoolName, Tier: c.Tier, TierName: c.TierName, Nodes: c.Nodes, GPUs: c.GPUs, Products: c.Products, Replaces: c.Replaces, Lends: c.Lends, Archs: c.Archs, Hardware: c.Hardware})
		}
		return out, err
	}
}

// spendSource reads LiteLLM's spend logs as rows of the usage profiles: successful requests of keys under a model alias, without LiteLLM's own health checks.
type spendSource struct{ lite *litellm.Client }

func (s spendSource) SpendRows(ctx context.Context, from, to time.Time) ([]usageprofiles.Row, error) {
	logs, err := s.lite.SpendLogs(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]usageprofiles.Row, 0, len(logs))
	for _, l := range logs {
		if l.Status != "" && !strings.EqualFold(l.Status, "success") {
			continue
		}
		if l.TeamAlias() == "litellm-internal-health-check" {
			continue
		}
		start, ok := litellm.SpendTime(l.StartTime)
		if !ok {
			continue
		}
		end, _ := litellm.SpendTime(l.EndTime)
		alias := l.ModelGroup
		if alias == "" {
			alias = l.Model
		}
		out = append(out, usageprofiles.Row{Key: l.APIKey, Alias: alias, Input: l.PromptTokens, Output: l.CompletionTokens, Start: start, End: end, UserAgent: l.UserAgent()})
	}
	return out, nil
}

// keyWindowHours is the key budget's window (LLM_KEY_BUDGET_WINDOW, e.g. 4h or 1d) in whole hours, 4 when it cannot be read.
func keyWindowHours(w string) int {
	w = strings.TrimSpace(w)
	if d, err := time.ParseDuration(w); err == nil && d >= time.Hour {
		return int(d / time.Hour)
	}
	if n, ok := strings.CutSuffix(w, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil && days > 0 {
			return days * 24
		}
	}
	return 4
}
