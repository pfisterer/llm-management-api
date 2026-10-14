package workspace

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The project's environment (docs/bauplan.md 10.12), run by every profile: a base of the catalogue (Config.Bases), a Git repository built on it by the GPU part (ImageBuilder, which hands each finished build to the project: ProjectImageBuilt), or an existing image — derived from a base (label in the registry) with everything, any other as it is, without the GPU kernel.

// ImageLabels reads an image's labels from its registry, for the check of finished project images; nil = no check.
type ImageLabels func(ctx context.Context, image string) (map[string]string, error)

// SetImageLabels adds the check of finished project images to Update.
func (s *Service) SetImageLabels(f ImageLabels) { s.labels = f }

// ImageBuild is the state of a project image's build from Git.
type ImageBuild struct {
	Status    string    `json:"status" enums:"building,ready,failed"`
	Image     string    `json:"image"`
	Commit    string    `json:"commit"`
	Branch    string    `json:"branch,omitempty"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ImageBuilder builds project images from Git (gpu.Service).
type ImageBuilder interface {
	BuildProjectImage(ctx context.Context, slug, base, gitURL, ref string, rebuild bool) (ImageBuild, error)
	ProjectImageBuild(ctx context.Context, slug string) (*ImageBuild, error)
	ProjectImageLog(ctx context.Context, slug string) (string, error)
	ForgetProjectImage(ctx context.Context, slug string) error
}

// SetImageBuilder adds project images from Git; without it a Git source is refused.
func (s *Service) SetImageBuilder(b ImageBuilder) { s.builds = b }

// BaseLabel marks notebook-base (dhbw-workspace-images) and every image derived from it; BaseVersions are the major versions a project image may be built on.
const BaseLabel = "cloud.dhbw.base"

var BaseVersions = []string{"notebook-v1"}

var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]+$`)

// applyProjectImage takes the environment of an update: from Git it starts (or keeps) the build on the base, and the project keeps its current image until the build is ready; an existing image is looked up in its registry unless unchanged — it must be pullable, and its label decides whether it is derived from a base.
func (s *Service) applyProjectImage(ctx context.Context, p *Project, cur Project) error {
	p.ProjectImage, p.ImageGitURL, p.ImageGitRef = strings.TrimSpace(p.ProjectImage), strings.TrimSpace(p.ImageGitURL), strings.TrimSpace(p.ImageGitRef)
	if p.ImageGitURL != "" {
		if s.builds == nil {
			return refuse("Projekt-Images aus Git sind hier nicht eingerichtet.")
		}
		base, _ := s.base(p.ImageBase)
		b, err := s.builds.BuildProjectImage(ctx, p.Slug, base.Image, p.ImageGitURL, p.ImageGitRef, false)
		if err != nil {
			return refuse("Projekt-Image: %v", err)
		}
		p.ProjectImage, p.ImageDerived = cur.ProjectImage, true
		if cur.ImageGitURL == "" || cur.ImageBase != p.ImageBase {
			p.ProjectImage = "" // until the build is ready, the base itself
		}
		if b.Status == "ready" {
			p.ProjectImage = b.Image
		}
		return nil
	}
	p.ImageGitRef = ""
	if cur.ImageGitURL != "" {
		s.forgetBuilds(ctx, p.Slug)
	}
	if p.ProjectImage == "" {
		p.ImageDerived = false
		return nil
	}
	if p.ProjectImage == cur.ProjectImage && cur.ImageGitURL == "" {
		p.ImageDerived = cur.ImageDerived
		return nil
	}
	if !imageRE.MatchString(p.ProjectImage) {
		return refuse("Projekt-Image %q ist kein Image-Name wie ghcr.io/org/kurs:2026.", p.ProjectImage)
	}
	if s.labels == nil {
		return nil
	}
	labels, err := s.labels(ctx, p.ProjectImage)
	if err != nil {
		return refuse("Image %s ist nicht abrufbar (es muss öffentlich sein): %v", p.ProjectImage, err)
	}
	p.ImageDerived = slices.Contains(BaseVersions, labels[BaseLabel])
	return nil
}

func (s *Service) forgetBuilds(ctx context.Context, slug string) {
	if s.builds != nil {
		_ = s.builds.ForgetProjectImage(ctx, slug) // a leftover build record only costs a row
	}
}

// RebuildProjectImage builds the project image from Git again: the ref's newest commit, or the same one anew after a failure.
func (s *Service) RebuildProjectImage(ctx context.Context, slug string) (ImageBuild, error) {
	p, err := s.store.Get(ctx, slug)
	if err != nil {
		return ImageBuild{}, err
	}
	if p.ImageGitURL == "" || s.builds == nil {
		return ImageBuild{}, refuse("Das Projekt-Image kommt nicht aus Git.")
	}
	base, _ := s.base(p.ImageBase)
	b, err := s.builds.BuildProjectImage(ctx, slug, base.Image, p.ImageGitURL, p.ImageGitRef, true)
	if err != nil {
		return ImageBuild{}, refuse("Projekt-Image: %v", err)
	}
	if b.Status == "ready" && b.Image != p.ProjectImage {
		return b, s.ProjectImageBuilt(ctx, slug, p.ImageGitURL, b.Image)
	}
	return b, nil
}

// ProjectImageLog is the tail of the project image's build log.
func (s *Service) ProjectImageLog(ctx context.Context, slug string) (string, error) {
	if s.builds == nil {
		return "", ErrNotFound
	}
	return s.builds.ProjectImageLog(ctx, slug)
}

// ProjectImageBuilt makes a finished build the project's image, as long as the project still builds from that repository.
func (s *Service) ProjectImageBuilt(ctx context.Context, slug, gitURL, image string) error {
	p, err := s.store.Get(ctx, slug)
	if err != nil || p.ImageGitURL == "" || p.ProjectImage == image || !sameRepo(p.ImageGitURL, gitURL) {
		return nil // project gone or changed meanwhile: nothing to follow
	}
	p.ProjectImage, p.UpdatedBy = image, "project-image-build"
	return s.store.Update(ctx, &p)
}

// sameRepo compares repository URLs loosely (the build keeps the canonical form: lower-case host, no .git, no trailing slash).
func sameRepo(a, b string) bool {
	norm := func(u string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git"))
	}
	return norm(a) == norm(b)
}

// ProjectImages are the images the projects people can start in now run, for pulling them onto the nodes ahead of time.
func (s *Service) ProjectImages(ctx context.Context) ([]string, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range list {
		if img := s.EffectiveImage(p); img != "" && p.Active(s.now()) && !slices.Contains(out, img) {
			out = append(out, img)
		}
	}
	return out, nil
}

// legacyProfiles are profiles that went into others when the environment moved to the project (14.10.2026): the GPU servers are one profile "gpu", whatever image.
var legacyProfiles = map[string]string{"pytorch": "gpu", "tensorflow": "gpu", "project-gpu": "gpu", "git-gpu": "gpu", "git-cpu": "cpu", "free": "cpu", "free-gpu": "gpu"}

// knownProfiles maps old profiles to their successors and drops what the catalogue no longer has, instead of refusing: the budget tree and old projects still carry them.
func (s *Service) knownProfiles(profiles []string) []string {
	var out []string
	for _, pr := range profiles {
		if n, ok := legacyProfiles[pr]; ok {
			pr = n
		}
		if _, ok := s.profile(pr); ok && !slices.Contains(out, pr) {
			out = append(out, pr)
		}
	}
	return out
}

// MigrateProfiles brings the stored projects to the current profiles once at start (knownProfiles); a project that had TensorFlow but not PyTorch gets the TensorFlow base.
func (s *Service) MigrateProfiles(ctx context.Context) error {
	list, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range list {
		profiles := s.knownProfiles(p.Profiles)
		base := p.ImageBase
		if base == "" && slices.Contains(p.Profiles, "tensorflow") && !slices.Contains(p.Profiles, "pytorch") {
			if _, ok := s.base("tensorflow"); ok {
				base = "tensorflow"
			}
		}
		if slices.Equal(profiles, p.Profiles) && base == p.ImageBase {
			continue
		}
		p.Profiles, p.ImageBase, p.UpdatedBy = profiles, base, "profile-migration"
		if err := s.store.Update(ctx, &p); err != nil {
			return err
		}
	}
	return nil
}

// base is a base of the catalogue by id; "" is the default one.
func (s *Service) base(id string) (Base, bool) {
	for _, b := range s.cfg.Bases {
		if b.ID == id || (id == "" && b.Default) {
			return b, true
		}
	}
	if id == "" && len(s.cfg.Bases) > 0 {
		return s.cfg.Bases[0], true
	}
	return Base{}, false
}

// EffectiveImage is what the project's servers run: its finished or built image, else its base; "" without a catalogue (the hub's own default).
func (s *Service) EffectiveImage(p Project) string {
	if p.ProjectImage != "" {
		return p.ProjectImage
	}
	b, _ := s.base(p.ImageBase)
	return b.Image
}

// ImageLabel names the project's environment for people: the base, plus the repository it is built from, or the finished image.
func (s *Service) ImageLabel(p Project) string {
	b, _ := s.base(p.ImageBase)
	switch {
	case p.ImageGitURL != "":
		return b.Name + " + " + strings.TrimPrefix(p.ImageGitURL, "https://")
	case p.ProjectImage != "":
		return p.ProjectImage
	}
	return b.Name
}
