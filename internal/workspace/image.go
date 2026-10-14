package workspace

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Project image (docs/bauplan.md 10.12): the image of "CPU + GPU-Kernel" (notebook and GPU kernel) and "Projekt-Image (GPU)" instead of notebook-base. Either a finished image derived from notebook-base, checked by its label in the registry, or built from a Git repository on notebook-base by the GPU part (ImageBuilder), which hands each finished build to the project (ProjectImageBuilt).

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
	BuildProjectImage(ctx context.Context, slug, gitURL, ref string, rebuild bool) (ImageBuild, error)
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

// applyProjectImage takes the project image of an update: from Git it starts (or keeps) the build, and the project keeps its current image until the build is ready; a finished image is checked for its label unless unchanged.
func (s *Service) applyProjectImage(ctx context.Context, p *Project, cur Project) error {
	p.ProjectImage, p.ImageGitURL, p.ImageGitRef = strings.TrimSpace(p.ProjectImage), strings.TrimSpace(p.ImageGitURL), strings.TrimSpace(p.ImageGitRef)
	if p.ImageGitURL != "" {
		if s.builds == nil {
			return refuse("Projekt-Images aus Git sind hier nicht eingerichtet.")
		}
		b, err := s.builds.BuildProjectImage(ctx, p.Slug, p.ImageGitURL, p.ImageGitRef, false)
		if err != nil {
			return refuse("Projekt-Image: %v", err)
		}
		p.ProjectImage = cur.ProjectImage
		if b.Status == "ready" {
			p.ProjectImage = b.Image
		}
		return nil
	}
	p.ImageGitRef = ""
	if cur.ImageGitURL != "" {
		s.forgetBuilds(ctx, p.Slug)
	}
	if p.ProjectImage == "" || (p.ProjectImage == cur.ProjectImage && cur.ImageGitURL == "") {
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
		return refuse("Projekt-Image %s ist nicht abrufbar (es muss öffentlich sein): %v", p.ProjectImage, err)
	}
	if !slices.Contains(BaseVersions, labels[BaseLabel]) {
		return refuse("Projekt-Image %s ist nicht von registry.gpu.services.dhbw.cloud/platform/notebook-base abgeleitet (Label %s fehlt). Für beliebige Images gibt es „Freies Image“.", p.ProjectImage, BaseLabel)
	}
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
	b, err := s.builds.BuildProjectImage(ctx, slug, p.ImageGitURL, p.ImageGitRef, true)
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

// ProjectImages are the project images of the projects people can start in now, for pulling them onto the nodes ahead of time.
func (s *Service) ProjectImages(ctx context.Context) ([]string, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range list {
		if p.ProjectImage != "" && p.Active(s.now()) && !slices.Contains(out, p.ProjectImage) {
			out = append(out, p.ProjectImage)
		}
	}
	return out, nil
}
