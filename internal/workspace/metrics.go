package workspace

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
	"github.com/pfisterer/llm-management-api/internal/usage"
)

// Metrics of the projects for the service's /metrics (webserver.MetricsRouter): the start decisions, counted as they happen, and what the database says about the projects, their budgets, storage and bases, computed when asked (the caller caches it briefly).
//
// No person appears in a label. Projects are labelled with their slug only (never the name, which people choose and may contain a name); their number is the budget tree's decision, and each has a handful of series. Profiles outside the catalogue count as "other", so a hub sending anything cannot make new series.

// StartResults are the outcomes of CheckStart as aiaas_workspace_starts_total counts them: ok, or what refused the start — project (none, unknown, not a member, ended, LLM only), image_error (the environment from Git is not built yet), no_profile (profile not allowed, or "CPU + GPU-Kernel" without a derived image), gpu_class (no GPUs or class not allowed or not chosen), limit (servers, cores, memory or GPUs per person), gpu_busy (the project's GPUs of the class are all in use), material_error (course material not fetchable), budget (a budget used up), error (the check itself failed) and other.
var StartResults = []string{"ok", "project", "image_error", "no_profile", "gpu_class", "limit", "gpu_busy", "material_error", "budget", "error", "other"}

// KernelProfile is the profile label of GPU kernels (pods of "CPU + GPU-Kernel" servers).
const KernelProfile = "gpu-kernel"

func newStartCounter(cfg Config) *promtext.CounterVec {
	c := promtext.NewCounterVec("aiaas_workspace_starts_total", "Start decisions of the service (JupyterHub's spawn check and GPU kernels) by profile and result: ok or the kind of refusal. Counted since the service started.", "profile", "result")
	profiles := []string{KernelProfile}
	for _, p := range cfg.Profiles {
		profiles = append(profiles, p.Slug)
	}
	for _, p := range profiles {
		for _, r := range StartResults {
			c.Init(p, r)
		}
	}
	return c
}

// startProfile is the profile label of a start: a catalogue profile, gpu-kernel for kernels, other for anything else.
func (s *Service) startProfile(r StartRequest) string {
	if r.Kernel {
		return KernelProfile
	}
	if _, ok := s.profile(r.Profile); ok {
		return r.Profile
	}
	return "other"
}

// countStart counts one decision of CheckStart.
func (s *Service) countStart(r StartRequest, err error) {
	result := "ok"
	var rf *Refusal
	switch {
	case err == nil:
	case errors.As(err, &rf):
		result = rf.Kind
		if !slices.Contains(StartResults, result) {
			result = "other"
		}
	default:
		result = "error"
	}
	s.starts.Inc(s.startProfile(r), result)
}

// WriteStartMetrics writes the start counter (in memory, never cached).
func (s *Service) WriteStartMetrics(w *promtext.Writer) { s.starts.WriteTo(w) }

// Metrics renders the projects' state from the database: projects by state, per project the end, the project-wide budgets with what is used of them, and the storage; the bases with their projects; the storage reports' age and the metadata engine's memory. Reads the projects and, once, the usage rows of the earliest window needed.
func (s *Service) Metrics(ctx context.Context) ([]byte, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var w promtext.Writer

	states := map[string]int{"active": 0, "llm_only": 0, "ended": 0}
	llm := map[string]int{"with_tokens": 0, "without_tokens": 0}
	envs := map[string]int{"base": 0, "git": 0, "image": 0}
	var unreachable, updates int
	var active []Project // with workspaces, people can start in them
	for _, p := range list {
		switch {
		case !p.Active(now):
			states["ended"]++
			continue
		case p.NoWorkspaces:
			states["llm_only"]++
		default:
			states["active"]++
			active = append(active, p)
			switch {
			case p.ImageGitURL != "":
				envs["git"]++
			case p.ProjectImage != "":
				envs["image"]++
				if _, broken := s.unpullable.Load(p.ProjectImage); broken {
					unreachable++
				}
				if p.ImageUpdate != "" && p.ImageUpdate != p.ImageDigest {
					updates++
				}
			default:
				envs["base"]++
			}
		}
		if p.LLMMTokens > 0 {
			llm["with_tokens"]++
		} else {
			llm["without_tokens"]++
		}
	}
	w.Header("aiaas_workspace_projects", "gauge", "Projects by state: active (people can start workspaces), llm_only (active, LLM tokens only) or ended (past their last day, not yet deleted by the budget tree).")
	for _, st := range []string{"active", "ended", "llm_only"} {
		w.Sample("aiaas_workspace_projects", float64(states[st]), "state", st)
	}
	w.Header("aiaas_llm_projects", "gauge", "Active projects (with or without workspaces) by whether the budget tree gave them LLM tokens (a LiteLLM team of their own).")
	for _, st := range []string{"with_tokens", "without_tokens"} {
		w.Sample("aiaas_llm_projects", float64(llm[st]), "state", st)
	}
	w.Header("aiaas_workspace_project_environments", "gauge", "Active projects with workspaces by environment: base (a base as it is), git (an image built from Git on a base) or image (a finished image).")
	for _, k := range []string{"base", "git", "image"} {
		w.Sample("aiaas_workspace_project_environments", float64(envs[k]), "kind", k)
	}
	w.Header("aiaas_workspace_project_images_unreachable", "gauge", "Active projects whose finished image did not answer in its registry at the last check (left out of the prepull).")
	w.Sample("aiaas_workspace_project_images_unreachable", float64(unreachable))
	w.Header("aiaas_workspace_project_image_updates_offered", "gauge", "Active projects whose finished image's tag moved and whose managers have not taken the new image yet.")
	w.Sample("aiaas_workspace_project_image_updates_offered", float64(updates))

	s.writeImages(&w, active)

	w.Header("aiaas_workspace_project_end_timestamp_seconds", "gauge", "Unix time after which nobody can start in the project (the end of its last day); only active projects with an end.")
	for _, p := range active {
		if p.EndsAt != nil {
			w.Sample("aiaas_workspace_project_end_timestamp_seconds", float64(p.EndsAt.Unix()+24*3600), "project", p.Slug)
		}
	}

	if err := s.writeBudgets(ctx, &w, active); err != nil {
		return nil, err
	}
	s.writeStorage(&w, list)
	s.writeBases(ctx, &w)
	return w.Bytes(), nil
}

// projectBudget is one project-wide budget as a series.
type projectBudget struct {
	project, unit, window, class, source string
	limit, used                          float64
}

// writeBudgets writes the project-wide budgets of active projects (per-person budgets are a person's share and say nothing about the project) with what the project used of each in its current window. All usage rows are read once, from the earliest window start any of them needs.
func (s *Service) writeBudgets(ctx context.Context, w *promtext.Writer, active []Project) error {
	now := s.now()
	var budgets []projectBudget
	earliest := usage.Day(now)
	type slot struct{ project, unit, window, class, source string }
	seen := map[slot]int{}
	for _, p := range active {
		for _, b := range p.Budgets {
			if b.PerPerson {
				continue
			}
			source := "project"
			if b.Tree {
				source = "tree"
			}
			k := slot{p.Slug, b.Unit, b.Window, b.GPUClass, source}
			if i, dup := seen[k]; dup {
				// Two equal budgets: the smaller one is the one that stops starts.
				budgets[i].limit = min(budgets[i].limit, b.Amount)
				continue
			}
			seen[k] = len(budgets)
			budgets = append(budgets, projectBudget{project: p.Slug, unit: b.Unit, window: b.Window, class: b.GPUClass, source: source, limit: b.Amount})
			if from := windowStart(b.Window, now, p.CreatedAt); from.Before(earliest) {
				earliest = from
			}
		}
	}
	if len(budgets) > 0 && s.usage != nil {
		rows, err := s.usage.Range(ctx, earliest, usage.Day(now))
		if err != nil {
			return err
		}
		created := map[string]Project{}
		for _, p := range active {
			created[p.Slug] = p
		}
		for i := range budgets {
			b := &budgets[i]
			from := windowStart(b.window, now, created[b.project].CreatedAt)
			for _, r := range rows {
				if r.Project == b.project && !r.Day.Before(from) {
					b.used += amountOf(r, Budget{Unit: b.unit, GPUClass: b.class})
				}
			}
		}
	}
	labels := func(b projectBudget) []string {
		return []string{"project", b.project, "unit", b.unit, "window", b.window, "gpu_class", b.class, "source", b.source}
	}
	w.Header("aiaas_workspace_project_budget_limit", "gauge", "A project-wide budget of an active project: amount per window in its unit (server_hours, vcpu_hours, ram_gb_hours, gpu_hours; gpu_class empty = all classes); source tree = the project's share from the budget tree, project = set by its managers.")
	for _, b := range budgets {
		w.Sample("aiaas_workspace_project_budget_limit", b.limit, labels(b)...)
	}
	w.Header("aiaas_workspace_project_budget_used", "gauge", "What the whole project used of that budget in its current window (calendar days in UTC: 24h = today, 7d and 30d including today, total = since the project was created).")
	for _, b := range budgets {
		w.Sample("aiaas_workspace_project_budget_used", b.used, labels(b)...)
	}
	return nil
}

// writeStorage writes the volumes of the projects with storage (until the tree deletes them) and the last storage reports of the hourly job.
func (s *Service) writeStorage(w *promtext.Writer, list []Project) {
	w.Header("aiaas_workspace_project_storage_limit_bytes", "gauge", "Size of the project's volume (ws-<slug>, storage from the budget tree); only projects with storage.")
	for _, p := range list {
		if p.StorageGB > 0 {
			w.Sample("aiaas_workspace_project_storage_limit_bytes", float64(p.StorageGB)*(1<<30), "project", p.Slug)
		}
	}
	w.Header("aiaas_workspace_project_storage_used_bytes", "gauge", "Space used on the project's volume at the last hourly report (juicefs quota); missing until a report named the volume.")
	for _, p := range list {
		if used, ok := s.storageUsed(p.Slug); ok && p.StorageGB > 0 {
			w.Sample("aiaas_workspace_project_storage_used_bytes", used*(1<<30), "project", p.Slug)
		}
	}
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	w.Header("aiaas_workspace_storage_report_timestamp_seconds", "gauge", "Unix time of the last report of the hourly storage job: dirs (juicefs quota list) and valkey (INFO memory of the JuiceFS metadata engine); missing before the first.")
	if !s.storage.dirsAt.IsZero() {
		w.Sample("aiaas_workspace_storage_report_timestamp_seconds", float64(s.storage.dirsAt.Unix()), "report", "dirs")
	}
	if !s.storage.valkeyAt.IsZero() {
		w.Sample("aiaas_workspace_storage_report_timestamp_seconds", float64(s.storage.valkeyAt.Unix()), "report", "valkey")
	}
	w.Header("aiaas_workspace_metadata_memory_bytes", "gauge", "Memory of the Valkey holding the JuiceFS metadata at the last report: used, peak, rss, max (maxmemory, 0 = none) and system (the node's RAM).")
	if v := s.storage.valkey; v != nil {
		for _, kv := range []struct {
			kind string
			v    int64
		}{{"used", v.UsedBytes}, {"peak", v.PeakBytes}, {"rss", v.RSSBytes}, {"max", v.MaxBytes}, {"system", v.SystemBytes}} {
			w.Sample("aiaas_workspace_metadata_memory_bytes", float64(kv.v), "kind", kv.kind)
		}
	}
}

// writeBases writes every version of the bases with how many active projects run it, and the end of life of deprecated ones.
func (s *Service) writeBases(ctx context.Context, w *promtext.Writer) {
	bases := s.basesInUse(ctx)
	w.Header("aiaas_base_projects", "gauge", "Active projects running a version of a base (the base itself or an image built on it from Git), by version, family and status: default (new projects get it), current or deprecated (runs out).")
	for _, b := range bases {
		status := "current"
		switch {
		case b.Deprecated():
			status = "deprecated"
		case b.Default:
			status = "default"
		}
		family := b.Family
		if family == "" {
			family = familyOf(b.ID)
		}
		w.Sample("aiaas_base_projects", float64(b.Projects), "base", b.ID, "family", family, "status", status)
	}
	w.Header("aiaas_base_end_of_life_timestamp_seconds", "gauge", "Unix time of a deprecated version's end of life, when its remaining projects move to its successor; only deprecated versions with a date.")
	for _, b := range bases {
		if !b.Deprecated() || b.EndOfLife == "" {
			continue
		}
		if t, err := time.Parse(time.DateOnly, b.EndOfLife); err == nil {
			w.Sample("aiaas_base_end_of_life_timestamp_seconds", float64(t.Unix()), "base", b.ID)
		}
	}
}

// MetricsProfiles are the profile labels the running servers may carry: the catalogue's, gpu-kernel and default (pods without the label).
func (s *Service) MetricsProfiles() []string {
	out := []string{KernelProfile, "default"}
	for _, p := range s.cfg.Profiles {
		if !slices.Contains(out, p.Slug) {
			out = append(out, p.Slug)
		}
	}
	return out
}

// writeImages writes what the checks know about the images of active projects (image_checks.go): each project's download, the downloads the prepull puts on every node of an architecture (each image once, only those of known size: the bases are not counted), the projects with warnings, and Harbor's scans. Compared with the nodes' free image filesystem (node_filesystem_avail_bytes) it tells whether the prepull still fits.
func (s *Service) writeImages(w *promtext.Writer, active []Project) {
	w.Header("aiaas_workspace_project_image_bytes", "gauge", "Download of the image an active project runs (its compressed layers), when known: finished images and builds from Git.")
	prepull := map[string]float64{"amd64": 0, "arm64": 0}
	seen := map[string]bool{}
	warned := 0
	for _, p := range active {
		if len(p.ImageWarnings) > 0 {
			warned++
		}
		if p.ImageSize <= 0 {
			continue
		}
		w.Sample("aiaas_workspace_project_image_bytes", float64(p.ImageSize), "project", p.Slug)
		img := s.EffectiveImage(p)
		if _, broken := s.unpullable.Load(p.ProjectImage); seen[img] || (broken && finishedImage(p)) {
			continue
		}
		seen[img] = true
		for _, a := range s.EffectiveArchs(p) {
			if _, ok := prepull[a]; ok {
				prepull[a] += float64(p.ImageSize)
			}
		}
	}
	w.Header("aiaas_workspace_prepull_project_image_bytes", "gauge", "Sum of the downloads of the project images the prepull keeps on every node of an architecture (each image once; the bases are not included).")
	for _, a := range []string{"amd64", "arm64"} {
		w.Sample("aiaas_workspace_prepull_project_image_bytes", prepull[a], "arch", a)
	}
	w.Header("aiaas_workspace_project_images_warned", "gauge", "Active projects whose image got warnings from the checks (root or another user, HOME elsewhere, own entrypoint, jupyterhub-singleuser not recognisable, large).")
	w.Sample("aiaas_workspace_project_images_warned", float64(warned))
	w.Header("aiaas_workspace_project_image_vulnerabilities", "gauge", "Vulnerabilities with a fix in the image an active project runs, by severity, from Harbor's last finished scan (Trivy); only images in Harbor that were scanned.")
	for _, p := range active {
		if p.ImageScan != nil && p.ImageScan.Status == "Success" {
			w.Sample("aiaas_workspace_project_image_vulnerabilities", float64(p.ImageScan.Critical), "project", p.Slug, "severity", "critical")
			w.Sample("aiaas_workspace_project_image_vulnerabilities", float64(p.ImageScan.High), "project", p.Slug, "severity", "high")
		}
	}
}
