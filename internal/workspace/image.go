package workspace

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The project's environment (docs/bauplan.md 10.12), run by every profile: a base of the catalogue (Config.Bases), a Git repository built on it by the GPU part (ImageBuilder, which hands each finished build to the project: ProjectImageBuilt), or an existing image — derived from a base (label in the registry) with everything, any other as it is, without the GPU kernel. An existing image runs at the digest its tag had when it was taken (ImageDigest); a newer one is offered, not taken silently (CheckProjectImages).

// RegistryImage is an image as its registry has it now: the digest its reference points at, its labels and its architectures.
type RegistryImage struct {
	Digest string
	Labels map[string]string
	Archs  []string
}

// ArchImage is an image with the architectures it runs on, for the prepull per architecture; Base marks a base of the catalogue (one of ours, which can run the prepull's idle container); PullSecret is the project's pull secret for a private image.
type ArchImage struct {
	Image      string
	Archs      []string
	Base       bool
	PullSecret string
}

// archsOr is a list of architectures, amd64 when it names none (images and bases from before architectures were known).
func archsOr(archs []string) []string {
	if len(archs) == 0 {
		return []string{"amd64"}
	}
	return archs
}

// ImageInspector looks an image up in its registry (oci.Client.Inspect), with the project's access to that registry if it has one; nil = no check, the image runs by its tag.
type ImageInspector func(ctx context.Context, image string, auth *Basic) (RegistryImage, error)

// SetImageInspector adds the check and pinning of finished project images.
func (s *Service) SetImageInspector(f ImageInspector) { s.inspect = f }

// pinned is the image fixed to a digest ("repo:tag@sha256:…").
func pinned(image, digest string) string {
	if at := strings.LastIndex(image, "@"); at >= 0 {
		image = image[:at]
	}
	return image + "@" + digest
}

// derivedFrom tells from an image's labels whether it is derived from a base.
func derivedFrom(labels map[string]string) bool {
	return slices.Contains(BaseVersions, labels[BaseLabel])
}

// ImageBuild is the state of a project image's build from Git.
type ImageBuild struct {
	Status    string    `json:"status" enums:"building,ready,failed"`
	Image     string    `json:"image"`
	Commit    string    `json:"commit"`
	Branch    string    `json:"branch,omitempty"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BuildLog is the tail of a build's log and what its pod is doing (waiting for a node, pulling the image, running, ended), for the people waiting for it; Since is when it started running.
type BuildLog struct {
	Log   string     `json:"log"`
	State string     `json:"state,omitempty"`
	Since *time.Time `json:"since,omitempty"`
}

// ImageBuilder builds project images from Git (gpu.Service).
type ImageBuilder interface {
	BuildProjectImage(ctx context.Context, slug, base, gitURL, ref string, rebuild bool) (ImageBuild, error)
	ProjectImageBuild(ctx context.Context, slug string) (*ImageBuild, error)
	ProjectImageLog(ctx context.Context, slug string) (BuildLog, error)
	ForgetProjectImage(ctx context.Context, slug string) error
	// CheckGitBranch checks a repository for course material: reachable (public, or with the project's access auth), on an allowed host, with that branch (empty = its default branch); it returns the repository's canonical address.
	CheckGitBranch(ctx context.Context, gitURL, branch string, auth *Basic) (string, error)
}

// SetImageBuilder adds project images from Git; without it a Git source is refused.
func (s *Service) SetImageBuilder(b ImageBuilder) { s.builds = b }

// BaseLabel marks notebook-base (dhbw-workspace-images) and every image derived from it; BaseVersions are the major versions a project image may be built on.
const BaseLabel = "cloud.dhbw.base"

var BaseVersions = []string{"notebook-v1"}

var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]+$`)

// applyProjectImage takes the environment of an update: from Git it starts (or keeps) the build on the base, and the project keeps its current image until the build is ready; an existing image is looked up in its registry unless unchanged — it must be pullable, its digest is kept (ImageDigest), and its label decides whether it is derived from a base.
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
		p.ProjectImage, p.ImageDerived, p.ImageDigest, p.ImageUpdate = cur.ProjectImage, true, "", ""
		if cur.ImageGitURL == "" || cur.ImageBase != p.ImageBase {
			p.ProjectImage = "" // until the build is ready, the base itself
		}
		if b.Status == "ready" {
			p.ProjectImage = b.Image
		}
		return nil
	}
	p.ImageGitRef = ""
	p.ImageArchs = nil
	if cur.ImageGitURL != "" {
		s.forgetBuilds(ctx, p.Slug)
	}
	if p.ProjectImage == "" {
		p.ImageDerived, p.ImageDigest, p.ImageUpdate = false, "", ""
		return nil
	}
	if p.ProjectImage == cur.ProjectImage && cur.ImageGitURL == "" {
		p.ImageDerived, p.ImageDigest, p.ImageUpdate, p.ImageArchs = cur.ImageDerived, cur.ImageDigest, cur.ImageUpdate, cur.ImageArchs
		return nil
	}
	if !imageRE.MatchString(p.ProjectImage) {
		return refuse("Projekt-Image %q ist kein Image-Name wie ghcr.io/org/kurs:2026.", p.ProjectImage)
	}
	p.ImageDigest, p.ImageUpdate = "", ""
	if s.inspect == nil {
		return nil
	}
	img, err := s.inspect(ctx, p.ProjectImage, s.credentialFor(ctx, p.Slug, imageHost(p.ProjectImage)))
	if err != nil {
		return refuse("Image %s ist nicht abrufbar (öffentlich, oder mit einem Zugang des Projekts für %s): %v", p.ProjectImage, imageHost(p.ProjectImage), err)
	}
	p.ImageDigest, p.ImageDerived, p.ImageArchs = img.Digest, derivedFrom(img.Labels), img.Archs
	return nil
}

// applyMaterial checks the course material of an update when it changed, and keeps the repository's canonical address.
func (s *Service) applyMaterial(ctx context.Context, p *Project, cur Project) error {
	p.MaterialGitURL, p.MaterialGitRef = strings.TrimSpace(p.MaterialGitURL), strings.TrimSpace(p.MaterialGitRef)
	if p.MaterialGitURL == "" {
		p.MaterialGitRef = ""
		return nil
	}
	if p.MaterialGitURL == cur.MaterialGitURL && p.MaterialGitRef == cur.MaterialGitRef {
		return nil
	}
	if s.builds == nil {
		return refuse("Material aus Git ist hier nicht eingerichtet.")
	}
	canonical, err := s.builds.CheckGitBranch(ctx, p.MaterialGitURL, p.MaterialGitRef, s.credentialFor(ctx, p.Slug, hostOfGit(p.MaterialGitURL)))
	if err != nil {
		return refuse("Material: %v", err)
	}
	p.MaterialGitURL = canonical
	return nil
}

// imageHost is the registry of an image reference (Docker Hub for one without a host).
func imageHost(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	return normaliseHost(first)
}

// finishedImage tells whether the project runs an existing image (not built from Git).
func finishedImage(p Project) bool { return p.ProjectImage != "" && p.ImageGitURL == "" }

// CheckProjectImages looks up the tag of every finished project image again (run periodically): a new digest is offered at the project (ImageUpdate), or taken at once with ImageAutoUpdate, its label checked again either way; a project from before the pinning gets its current digest. An image whose registry does not answer is left out of the prepull until it answers again.
func (s *Service) CheckProjectImages(ctx context.Context) error {
	if s.inspect == nil {
		return nil
	}
	list, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range list {
		if !finishedImage(p) || !p.Active(s.now()) {
			continue
		}
		img, err := s.inspect(ctx, p.ProjectImage, s.credentialFor(ctx, p.Slug, imageHost(p.ProjectImage)))
		if err != nil {
			s.unpullable.Store(p.ProjectImage, true)
			errs = append(errs, err)
			continue
		}
		s.unpullable.Delete(p.ProjectImage)
		switch {
		case img.Digest == p.ImageDigest:
			if p.ImageUpdate == "" && (len(p.ImageArchs) > 0 || len(img.Archs) == 0) {
				continue
			}
			p.ImageUpdate, p.ImageArchs = "", img.Archs // the tag went back, or the architectures were not known yet
		case p.ImageDigest == "" || p.ImageAutoUpdate:
			p.ImageDigest, p.ImageDerived, p.ImageUpdate, p.ImageArchs = img.Digest, derivedFrom(img.Labels), "", img.Archs
		case img.Digest == p.ImageUpdate:
			continue
		default:
			p.ImageUpdate = img.Digest
		}
		p.UpdatedBy = "image-check"
		if err := s.store.Update(ctx, &p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// UpdateProjectImage takes the digest the finished image's tag points at now, its label checked again.
func (s *Service) UpdateProjectImage(ctx context.Context, slug, by string) (Project, error) {
	p, err := s.store.Get(ctx, slug)
	if err != nil {
		return Project{}, err
	}
	if !finishedImage(p) || s.inspect == nil {
		return Project{}, refuse("Das Projekt nutzt kein fertiges Image.")
	}
	img, err := s.inspect(ctx, p.ProjectImage, s.credentialFor(ctx, p.Slug, imageHost(p.ProjectImage)))
	if err != nil {
		return Project{}, refuse("Image %s ist nicht abrufbar (öffentlich, oder mit einem Zugang des Projekts für %s): %v", p.ProjectImage, imageHost(p.ProjectImage), err)
	}
	p.ImageDigest, p.ImageDerived, p.ImageUpdate, p.ImageArchs, p.UpdatedBy = img.Digest, derivedFrom(img.Labels), "", img.Archs, by
	return p, s.store.Update(ctx, &p)
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
func (s *Service) ProjectImageLog(ctx context.Context, slug string) (BuildLog, error) {
	if s.builds == nil {
		return BuildLog{}, ErrNotFound
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

// PrepullImages are the bases (first, in the order of the catalogue) and the images the projects people can start in now run, each with its architectures, for pulling them onto the nodes ahead of time.
func (s *Service) PrepullImages(ctx context.Context) ([]ArchImage, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []ArchImage
	add := func(img string, archs []string, base bool, secret string) {
		if img != "" && !slices.ContainsFunc(out, func(a ArchImage) bool { return a.Image == img }) {
			out = append(out, ArchImage{Image: img, Archs: archsOr(archs), Base: base, PullSecret: secret})
		}
	}
	for _, b := range s.cfg.Bases {
		add(b.Image, b.Archs, true, "")
	}
	for _, p := range list {
		if _, broken := s.unpullable.Load(p.ProjectImage); broken && finishedImage(p) {
			continue
		}
		if p.Active(s.now()) {
			secret := ""
			if finishedImage(p) && s.credentialFor(ctx, p.Slug, imageHost(p.ProjectImage)) != nil {
				secret = PullSecretName(p.Slug)
			}
			add(s.EffectiveImage(p), s.EffectiveArchs(p), false, secret)
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

// EffectiveImage is what the project's servers run: its finished image at its digest, its built image, else its base; "" without a catalogue (the hub's own default).
func (s *Service) EffectiveImage(p Project) string {
	if finishedImage(p) && p.ImageDigest != "" {
		return pinned(p.ProjectImage, p.ImageDigest)
	}
	if p.ProjectImage != "" {
		return p.ProjectImage
	}
	b, _ := s.base(p.ImageBase)
	return b.Image
}

// EffectiveArchs are the architectures the project's environment runs on: an existing image's own, a build from Git amd64 (built on the amd64 build nodes), a base its configured ones.
func (s *Service) EffectiveArchs(p Project) []string {
	switch {
	case finishedImage(p):
		return archsOr(p.ImageArchs)
	case p.ImageGitURL != "":
		return []string{"amd64"}
	}
	b, _ := s.base(p.ImageBase)
	return archsOr(b.Archs)
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
