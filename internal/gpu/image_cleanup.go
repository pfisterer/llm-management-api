package gpu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/oci"
	"github.com/pfisterer/llm-management-api/internal/promtext"
)

// Image cleanup in Harbor (docs/todo.md image_cleanup): one periodic sweep compares the artifacts in the Harbor project of the environments (envs) with the images still referred to, and deletes the rest. It covers every way an environment goes (a project deleted by an admin or by the budget tree after its grace days, a rebuild that forgets the older build, a crash between both) and old leftovers, so no deletion path has to call Harbor itself, and a failed delete is simply tried again at the next pass.
//
// Kept is every artifact one of whose tags, or whose digest, an environment record (all projects, any status) or a project (its image by tag, digest, offered update) names: images are shared between projects with the same repository and commit, so an image goes only once nobody names it. The build cache envs/buildcache:<repository> stays while any image of that repository is kept. Only the envs project is ever touched; the platform images and the bases are in platform. An artifact pushed or pulled within MinAge stays as well (a build that is just being saved, something unknown still using it), and the references are read again right before deleting, so an environment created during the pass keeps its image.

// HarborRepos is what the cleanup needs of Harbor (oci.Harbor).
type HarborRepos interface {
	Repositories(ctx context.Context, project string) ([]oci.HarborRepository, error)
	Artifacts(ctx context.Context, project, repo string) ([]oci.HarborArtifact, error)
	DeleteArtifact(ctx context.Context, project, repo, digest string) error
	DeleteRepository(ctx context.Context, project, repo string) error
}

// ImageCleanupConfig switches the cleanup: DryRun only logs what would go; MinAge is how long an artifact must have been neither pushed nor pulled; Interval how often the sweep runs.
type ImageCleanupConfig struct {
	Enabled  bool
	DryRun   bool
	MinAge   time.Duration
	Interval time.Duration
}

// buildCacheRepo is the repository of the build cache (job.go: R2D_BUILDKIT_CACHE), one tag per image repository.
const buildCacheRepo = "buildcache"

// CleanupResult is what one pass found and did.
type CleanupResult struct {
	At           time.Time
	DryRun       bool
	Repositories int // repositories in the project
	Artifacts    int // artifacts in them
	Referenced   int // kept: named by an environment or a project
	Young        int // kept: not referenced, but pushed or pulled within MinAge
	Orphaned     int // not referenced and older: deleted, or to be deleted in a dry run
	Deleted      int // artifacts deleted (a deleted repository counts its artifacts)
	Failed       int // deletes or listings that failed; tried again at the next pass
	OK           bool
}

// ImageCleanup is the sweep; Run runs it every Interval.
type ImageCleanup struct {
	svc     *Service
	harbor  HarborRepos
	project string
	cfg     ImageCleanupConfig
	refs    func(ctx context.Context) ([]string, error)
	now     func() time.Time
}

// NewImageCleanup makes the sweep over the envs project; projectRefs are the images the projects refer to (workspace.Service.ReferencedImages), the environment records are read from the service's store.
func (s *Service) NewImageCleanup(h HarborRepos, cfg ImageCleanupConfig, projectRefs func(ctx context.Context) ([]string, error)) *ImageCleanup {
	if cfg.MinAge <= 0 {
		cfg.MinAge = 7 * 24 * time.Hour
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	return &ImageCleanup{svc: s, harbor: h, project: s.cfg.EnvsProject, cfg: cfg, refs: projectRefs, now: time.Now}
}

// references are the tags and digests per repository of the envs project that something names, and the repositories named at all.
type references struct {
	tags    map[string]map[string]bool
	digests map[string]map[string]bool
}

func (r references) repo(name string) bool { return len(r.tags[name]) > 0 || len(r.digests[name]) > 0 }

func (r references) keeps(repo string, a oci.HarborArtifact) bool {
	if repo == buildCacheRepo {
		for _, t := range a.Tags {
			if r.repo(t) {
				return true
			}
		}
		return false
	}
	if r.digests[repo][a.Digest] {
		return true
	}
	for _, t := range a.Tags {
		if r.tags[repo][t] {
			return true
		}
	}
	return false
}

// readReferences reads every environment record and the projects' images. The registry host is not compared (an image named through another name of the same Harbor still counts): only the project and repository matter, which keeps more rather than less.
func (c *ImageCleanup) readReferences(ctx context.Context) (references, error) {
	envs, err := c.svc.store.List(ctx, "")
	if err != nil {
		return references{}, fmt.Errorf("environments: %w", err)
	}
	images := make([]string, 0, len(envs))
	for _, e := range envs {
		images = append(images, e.Image)
	}
	if c.refs != nil {
		more, err := c.refs(ctx)
		if err != nil {
			return references{}, fmt.Errorf("projects: %w", err)
		}
		images = append(images, more...)
	}
	r := references{tags: map[string]map[string]bool{}, digests: map[string]map[string]bool{}}
	add := func(m map[string]map[string]bool, repo, v string) {
		if m[repo] == nil {
			m[repo] = map[string]bool{}
		}
		m[repo][v] = true
	}
	for _, img := range images {
		img = strings.TrimSpace(img)
		if img == "" {
			continue
		}
		// "repo:tag@digest": the tag counts as well as the digest.
		if at := strings.LastIndex(img, "@"); at >= 0 {
			if tr, err := oci.ParseRef(img[:at]); err == nil && tr.Ref != "latest" {
				if repo, ok := strings.CutPrefix(tr.Repo, c.project+"/"); ok {
					add(r.tags, repo, tr.Ref)
				}
			}
		}
		ref, err := oci.ParseRef(img)
		if err != nil {
			continue
		}
		repo, ok := strings.CutPrefix(ref.Repo, c.project+"/")
		if !ok {
			continue
		}
		if strings.HasPrefix(ref.Ref, "sha256:") {
			add(r.digests, repo, ref.Ref)
		} else {
			add(r.tags, repo, ref.Ref)
		}
	}
	return r, nil
}

// orphan is a repository's artifacts to delete; whole = all of them, so the repository goes.
type orphan struct {
	repo      string
	artifacts []oci.HarborArtifact
	whole     bool
	empty     bool // no artifacts at all (a leftover repository)
}

// Sweep runs one pass. A failure to read the references deletes nothing; a failed listing or delete is counted and logged, and the pass goes on.
func (c *ImageCleanup) Sweep(ctx context.Context) CleanupResult {
	log := c.svc.log
	res := CleanupResult{At: c.now().UTC(), DryRun: c.cfg.DryRun}
	if c.project == "" {
		log.Warnw("image cleanup: no envs project configured, nothing done")
		return c.record(res)
	}
	refs, err := c.readReferences(ctx)
	if err != nil {
		log.Warnw("image cleanup: references not readable, nothing deleted", "error", err)
		res.Failed++
		return c.record(res)
	}
	repos, err := c.harbor.Repositories(ctx, c.project)
	if err != nil {
		log.Warnw("image cleanup: repositories not readable", "project", c.project, "error", err)
		res.Failed++
		return c.record(res)
	}
	res.Repositories = len(repos)
	cutoff := c.now().Add(-c.cfg.MinAge)
	var orphans []orphan
	for _, r := range repos {
		if r.ArtifactCount == 0 {
			if !refs.repo(r.Name) && r.UpdateTime.Before(cutoff) {
				orphans = append(orphans, orphan{repo: r.Name, whole: true, empty: true})
			}
			continue
		}
		arts, err := c.harbor.Artifacts(ctx, c.project, r.Name)
		if err != nil {
			log.Warnw("image cleanup: artifacts not readable", "repository", c.project+"/"+r.Name, "error", err)
			res.Failed++
			continue
		}
		res.Artifacts += len(arts)
		o := orphan{repo: r.Name}
		for _, a := range arts {
			switch {
			case refs.keeps(r.Name, a):
				res.Referenced++
			case a.PushTime.After(cutoff) || a.PullTime.After(cutoff):
				res.Young++
			default:
				o.artifacts = append(o.artifacts, a)
			}
		}
		if len(o.artifacts) == 0 {
			continue
		}
		o.whole = len(o.artifacts) == len(arts) && !refs.repo(r.Name)
		orphans = append(orphans, o)
	}
	for _, o := range orphans {
		res.Orphaned += len(o.artifacts)
	}
	if len(orphans) == 0 {
		res.OK = res.Failed == 0
		log.Infow("image cleanup: nothing to delete", "project", c.project, "repositories", res.Repositories, "artifacts", res.Artifacts, "referenced", res.Referenced, "young", res.Young, "dry_run", res.DryRun)
		return c.record(res)
	}
	// Read again right before deleting: an environment created or a project image saved during the pass keeps its image.
	if !c.cfg.DryRun {
		if refs, err = c.readReferences(ctx); err != nil {
			log.Warnw("image cleanup: references not readable, nothing deleted", "error", err)
			res.Failed++
			return c.record(res)
		}
	}
	for _, o := range orphans {
		c.delete(ctx, refs, o, &res)
	}
	res.OK = res.Failed == 0
	verb := "deleted"
	if res.DryRun {
		verb = "would delete (dry run)"
	}
	log.Infow("image cleanup: "+verb, "project", c.project, "repositories", res.Repositories, "artifacts", res.Artifacts, "referenced", res.Referenced, "young", res.Young, "orphaned", res.Orphaned, "deleted", res.Deleted, "failed", res.Failed, "min_age", c.cfg.MinAge.String())
	return c.record(res)
}

func (c *ImageCleanup) delete(ctx context.Context, refs references, o orphan, res *CleanupResult) {
	log := c.svc.log
	name := c.project + "/" + o.repo
	var keep []oci.HarborArtifact
	for _, a := range o.artifacts {
		if refs.keeps(o.repo, a) {
			keep = append(keep, a)
		}
	}
	if len(keep) > 0 || refs.repo(o.repo) {
		o.whole = false
	}
	if o.whole {
		if c.cfg.DryRun {
			log.Infow("image cleanup: would delete repository (dry run)", "repository", name, "artifacts", len(o.artifacts), "tags", tagsOf(o.artifacts), "last_used", lastUsed(o.artifacts))
			return
		}
		if err := c.harbor.DeleteRepository(ctx, c.project, o.repo); err != nil {
			log.Warnw("image cleanup: deleting a repository failed, again at the next pass", "repository", name, "error", err)
			res.Failed++
			return
		}
		res.Deleted += len(o.artifacts)
		log.Infow("image cleanup: repository deleted", "repository", name, "artifacts", len(o.artifacts), "tags", tagsOf(o.artifacts))
		return
	}
	for _, a := range o.artifacts {
		if refs.keeps(o.repo, a) {
			continue
		}
		if c.cfg.DryRun {
			log.Infow("image cleanup: would delete artifact (dry run)", "repository", name, "digest", a.Digest, "tags", a.Tags, "last_used", lastUsed([]oci.HarborArtifact{a}))
			continue
		}
		if err := c.harbor.DeleteArtifact(ctx, c.project, o.repo, a.Digest); err != nil {
			log.Warnw("image cleanup: deleting an artifact failed, again at the next pass", "repository", name, "digest", a.Digest, "error", err)
			res.Failed++
			continue
		}
		res.Deleted++
		log.Infow("image cleanup: artifact deleted", "repository", name, "digest", a.Digest, "tags", a.Tags)
	}
}

func tagsOf(arts []oci.HarborArtifact) []string {
	var out []string
	for _, a := range arts {
		out = append(out, a.Tags...)
	}
	return out
}

// lastUsed is the latest push or pull of the artifacts.
func lastUsed(arts []oci.HarborArtifact) time.Time {
	var t time.Time
	for _, a := range arts {
		for _, x := range []time.Time{a.PushTime, a.PullTime} {
			if x.After(t) {
				t = x
			}
		}
	}
	return t
}

func (c *ImageCleanup) record(res CleanupResult) CleanupResult {
	c.svc.cleanupLast.Store(&res)
	return res
}

// Run sweeps every Interval until ctx ends, the first time a few minutes after the start (the stores and the projects are settled by then).
func (c *ImageCleanup) Run(ctx context.Context, firstDelay time.Duration) {
	c.svc.log.Infow("image cleanup in Harbor on", "project", c.project, "dry_run", c.cfg.DryRun, "min_age", c.cfg.MinAge.String(), "interval", c.cfg.Interval.String())
	t := time.NewTimer(firstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.Sweep(ctx)
		t.Reset(c.cfg.Interval)
	}
}

// writeCleanupMetrics writes the last pass of the image cleanup, if one ran.
func (s *Service) writeCleanupMetrics(w *promtext.Writer) {
	res := s.cleanupLast.Load()
	if res == nil {
		return
	}
	dry := "false"
	if res.DryRun {
		dry = "true"
	}
	w.Header("aiaas_image_cleanup_last_run_timestamp_seconds", "gauge", "When the image cleanup in Harbor (project envs) last ran.")
	w.Sample("aiaas_image_cleanup_last_run_timestamp_seconds", float64(res.At.Unix()), "dry_run", dry)
	ok := 0.0
	if res.OK {
		ok = 1
	}
	w.Header("aiaas_image_cleanup_last_run_ok", "gauge", "1 when the last pass of the image cleanup read everything and every delete succeeded; failures are tried again at the next pass.")
	w.Sample("aiaas_image_cleanup_last_run_ok", ok, "dry_run", dry)
	w.Header("aiaas_image_cleanup_artifacts", "gauge", "Artifacts in the Harbor project envs at the last pass of the image cleanup, by state: referenced (named by an environment or a project), young (unreferenced, but pushed or pulled within the minimum age), orphaned (unreferenced and older: deleted, or due in a dry run), deleted, failed (deletes and listings).")
	for _, kv := range []struct {
		state string
		n     int
	}{{"referenced", res.Referenced}, {"young", res.Young}, {"orphaned", res.Orphaned}, {"deleted", res.Deleted}, {"failed", res.Failed}} {
		w.Sample("aiaas_image_cleanup_artifacts", float64(kv.n), "state", kv.state, "dry_run", dry)
	}
}
