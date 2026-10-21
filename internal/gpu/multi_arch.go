package gpu

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Project images from Git for several architectures (docs/todo.md multi_arch): a base built for amd64 and arm64 (images.yaml platforms) gives an image for both where Config.BuildPlatforms names arm64. Each architecture is built natively in its own job (buildJob: amd64 on the build nodes, arm64 on an arm64 node; no emulation) and pushed to the image's repository without a tag, its digest at the end of its log; once the amd64 build is done and the others are done or out of time (archGrace), an index job (indexJob) joins the variants that succeeded under the image's tag. In Harbor the variants are then children of the index: not listed on their own, kept and deleted with it (image_cleanup.go). Until the index is written they are untagged; Harbor's nightly garbage collection (delete_untagged, tasks/harbor.yaml) can take one that waits across it, the index job then fails and the build is started again by hand. The amd64 build decides: if it fails, the build fails; another architecture failing leaves an image without it, its GPU classes are then not offered for the project (the architectures come from the index, oci.Client.Inspect), and the build's message says why.

// archGrace is how long the builds of the other architectures may take once the amd64 build is done: an arm64 node that is busy or gone does not hold up the image for longer.
const archGrace = 30 * time.Minute

// archJobName is the job of one architecture of a build for several.
func archJobName(slug, tag, arch string) string { return jobName(slug, tag+"-"+arch) }

// indexJobName is the job that writes the index of a build for several architectures.
func indexJobName(slug, tag string) string { return jobName(slug, tag+"-index") }

// repoOf is an image's repository, without its tag.
func repoOf(image string) string {
	repo, _ := splitImage(image)
	return repo
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// variant is the variant one architecture's job pushed (repository@digest, from the "dhbw-digest: " line at the end of its log); "" when the log does not name one.
func (s *Service) variant(ctx context.Context, e Environment, job string) string {
	log, err := s.builds.jobLog(ctx, s.cfg.BuildNamespace, job, 20)
	if err != nil {
		return ""
	}
	d := ""
	for _, line := range strings.Split(log, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "dhbw-digest: "); ok && digestRE.MatchString(v) {
			d = v
		}
	}
	if d == "" {
		return ""
	}
	return repoOf(e.Image) + "@" + d
}

// platformsOf are the architectures of a build for several, amd64 first; nil for an amd64 build.
func platformsOf(e Environment) []string {
	if e.Platforms == "" {
		return nil
	}
	return strings.Split(e.Platforms, ",")
}

// baseArchs are the architectures a base is built for: its own entry, else its repository's (another release of it), else amd64.
func (s *Service) baseArchs(base string) []string {
	repo, _ := splitImage(base)
	var found []string
	for _, b := range s.bases {
		if b.image == base {
			return b.archs
		}
		if r, _ := splitImage(b.image); r == repo && found == nil {
			found = b.archs
		}
	}
	if found == nil {
		return []string{"amd64"}
	}
	return found
}

// buildPlatforms are the architectures a build on base makes: amd64, then those of Config.BuildPlatforms the base has (TensorFlow is amd64 only: no CUDA wheels for arm64).
func (s *Service) buildPlatforms(base string) []string {
	out := []string{"amd64"}
	archs := s.baseArchs(base)
	for _, a := range s.cfg.BuildPlatforms {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && !slices.Contains(out, a) && slices.Contains(archs, a) {
			out = append(out, a)
		}
	}
	return out
}

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// refreshPlatforms follows a build for several architectures: the amd64 job, then the others (until archGrace after amd64), then the index job; ready when the index is written.
func (s *Service) refreshPlatforms(ctx context.Context, e Environment) Environment {
	ns := s.cfg.BuildNamespace
	slug, tag := slugOf(e.Image), tagOf(e.Image)
	index := indexJobName(slug, tag)
	st, err := s.builds.jobStatus(ctx, ns, index)
	switch {
	case err == nil && st.Succeeded > 0:
		return s.settle(ctx, e, StatusReady, e.Message) // the message says which architectures are missing
	case err == nil && st.Failed > 0:
		return s.settle(ctx, e, StatusFailed, "Die Architekturen ließen sich nicht zu einem Image zusammenführen (Index-Job "+index+").")
	case err == nil:
		return e // the index is being written
	case !errors.Is(err, ErrNotFound):
		s.log.Warnw("build status", "job", index, "error", err)
		return e
	}
	amd, err := s.builds.jobStatus(ctx, ns, e.JobName)
	switch {
	case errors.Is(err, ErrNotFound):
		// Jobs gone (cleaned up after a day): decide by the registry.
		ok, rerr := s.images.exists(ctx, s.cfg.EnvsProject, slug, tag)
		switch {
		case rerr != nil:
			return e
		case ok:
			return s.settle(ctx, e, StatusReady, e.Message)
		default:
			return s.settle(ctx, e, StatusFailed, "Build nicht mehr vorhanden, Image fehlt.")
		}
	case err != nil:
		s.log.Warnw("build status", "job", e.JobName, "error", err)
		return e
	case amd.Failed > 0:
		return s.settle(ctx, e, StatusFailed, s.failure(ctx, e.JobName, amd))
	case amd.Succeeded == 0:
		return e
	}
	// amd64 is there: the other architectures until archGrace after it.
	first := s.variant(ctx, e, e.JobName)
	if first == "" {
		return s.settle(ctx, e, StatusFailed, "Der amd64-Build hat kein Image gemeldet (Zeile dhbw-digest im Build-Log fehlt).")
	}
	done := s.now()
	if amd.CompletionTime != nil {
		done = *amd.CompletionTime
	}
	built, variants := []string{"amd64"}, []string{first}
	var missing []string
	for _, arch := range platformsOf(e)[1:] {
		name := archJobName(slug, tag, arch)
		st, err := s.builds.jobStatus(ctx, ns, name)
		switch {
		case errors.Is(err, ErrNotFound):
			missing = append(missing, arch+": Build nicht mehr vorhanden")
		case err != nil:
			s.log.Warnw("build status", "job", name, "error", err)
			return e
		case st.Succeeded > 0:
			v := s.variant(ctx, e, name)
			if v == "" {
				missing = append(missing, arch+": kein Image gemeldet")
				break
			}
			built, variants = append(built, arch), append(variants, v)
		case st.Failed > 0:
			missing = append(missing, arch+": "+s.failure(ctx, name, st))
		case s.now().Sub(done) < archGrace:
			return e
		default:
			// Waiting for a node or still building: given up, so the image does not wait for it.
			if err := s.builds.deleteJob(ctx, ns, name); err != nil {
				s.log.Warnw("build of an architecture not stopped", "job", name, "error", err)
			}
			missing = append(missing, fmt.Sprintf("%s: %d Minuten nach amd64 noch nicht fertig, abgebrochen", arch, int(archGrace.Minutes())))
		}
	}
	err = s.builds.createJob(ctx, ns, indexJob(s.cfg, index, slug, e.Image, variants))
	if err != nil && !errors.Is(err, errAlreadyExists) {
		s.log.Warnw("index job not created", "job", index, "error", err)
		return e
	}
	e.Message = ""
	if len(missing) > 0 {
		e.Message = "Nur für " + strings.Join(built, ", ") + " gebaut, GPU-Klassen anderer Architekturen werden nicht angeboten (" + strings.Join(missing, "; ") + ")."
		s.log.Warnw("project image without some architectures", "image", e.Image, "built", built, "missing", missing)
	}
	saved, err := s.store.Save(ctx, e)
	if err != nil {
		s.log.Warnw("save environment", "id", e.ID, "error", err)
		return e
	}
	return saved
}

// settle ends a build for several architectures: ready (the project follows, SetBuildReady) or failed, with the message.
func (s *Service) settle(ctx context.Context, e Environment, status Status, msg string) Environment {
	e.Status, e.Message = status, msg
	if status == StatusReady && s.ready != nil {
		if err := s.ready(ctx, e); err != nil {
			s.log.Warnw("project image: project not moved to the new build", "image", e.Image, "error", err)
			return e
		}
	}
	saved, err := s.store.Save(ctx, e)
	if err != nil {
		s.log.Warnw("save environment", "id", e.ID, "error", err)
		return e
	}
	return saved
}
