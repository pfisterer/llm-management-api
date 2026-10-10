package gpu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

var (
	ErrInvalidRepo = errors.New("Repository nicht verwendbar")
	ErrForbidden   = errors.New("not your environment")
	ErrNotReady    = errors.New("environment not ready")
)

// builds starts and watches build jobs; registry answers whether an image
// exists; servers starts JupyterHub servers. Interfaces so the service can be
// tested without the cluster.
type builds interface {
	createJob(ctx context.Context, ns string, job map[string]any) error
	jobStatus(ctx context.Context, ns, name string) (jobStatus, error)
	deleteJob(ctx context.Context, ns, name string) error
	jobLog(ctx context.Context, ns, job string, tail int) (string, error)
}

type images interface {
	exists(ctx context.Context, project, repo, tag string) (bool, error)
	newestBuilder(ctx context.Context) (string, error)
}

type servers interface {
	user(ctx context.Context, name string) (*hubUser, error)
	ensureUser(ctx context.Context, name string) error
	startServer(ctx context.Context, user, server string, options map[string]any) error
	stopServer(ctx context.Context, user, server string, remove bool) error
}

type refResolver func(ctx context.Context, repoURL string) (gitRefs, error)

// Service is the GPU part's logic.
type Service struct {
	cfg     Config
	store   Store
	builds  builds
	images  images
	servers servers
	refs    refResolver
	log     *zap.SugaredLogger
}

// New connects to the GPU cluster's APIs.
func New(cfg Config, store Store, log *zap.SugaredLogger) (*Service, error) {
	k, err := newKube(cfg.KubeAPIURL, cfg.KubeToken, cfg.KubeCA)
	if err != nil {
		return nil, err
	}
	git := newGitClient()
	return newService(cfg, store, k, newRegistry(cfg.Registry), newHub(cfg.JupyterURL, cfg.JupyterAPIToken),
		func(ctx context.Context, u string) (gitRefs, error) { return fetchRefs(ctx, git, u) }, log), nil
}

func newService(cfg Config, store Store, b builds, i images, s servers, r refResolver, log *zap.SugaredLogger) *Service {
	if cfg.BuildNamespace == "" {
		cfg.BuildNamespace = "env-build"
	}
	if cfg.EnvsProject == "" {
		cfg.EnvsProject = "envs"
	}
	if cfg.BuilderSecret == "" {
		cfg.BuilderSecret = "harbor-envs-builder"
	}
	if len(cfg.GitHosts) == 0 {
		cfg.GitHosts = []string{"github.com", "gitlab.com"}
	}
	return &Service{cfg: cfg, store: store, builds: b, images: i, servers: s, refs: r, log: log}
}

// JupyterURL is shown to people with GPU access.
func (s *Service) JupyterURL() string { return s.cfg.JupyterURL }

// Tier returns the configured GPU tier (zero value if unknown).
func (s *Service) Tier(name string) Tier {
	for _, t := range s.cfg.Tiers {
		if t.Name == name {
			return t
		}
	}
	return Tier{}
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// normaliseRepo checks a repository URL: https, an allowed host, no
// credentials, at least owner/name. Returns the canonical URL and the image
// repository name (slug) in Harbor envs/.
func (s *Service) normaliseRepo(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("%w: nur öffentliche https-Adressen ohne Zugangsdaten", ErrInvalidRepo)
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, h := range s.cfg.GitHosts {
		if host == strings.ToLower(h) {
			allowed = true
		}
	}
	if !allowed {
		return "", "", fmt.Errorf("%w: %s ist nicht zugelassen (zugelassen: %s)", ErrInvalidRepo, host, strings.Join(s.cfg.GitHosts, ", "))
	}
	path := strings.Trim(strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git"), "/")
	if strings.Count(path, "/") < 1 {
		return "", "", fmt.Errorf("%w: erwartet https://%s/<besitzer>/<name>", ErrInvalidRepo, host)
	}
	canonical := "https://" + host + "/" + path
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(host+"-"+path), "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	return canonical, slug, nil
}

// Create adds an environment for owner: resolve the ref, reuse the image if it
// exists, otherwise start a build.
func (s *Service) Create(ctx context.Context, owner, gitURL, ref, name string) (Environment, error) {
	canonical, slug, err := s.normaliseRepo(gitURL)
	if err != nil {
		return Environment{}, err
	}
	refs, err := s.refs(ctx, canonical)
	if err != nil {
		return Environment{}, fmt.Errorf("%w: %v", ErrInvalidRepo, err)
	}
	commit, branch, err := refs.resolve(ref)
	if err != nil {
		return Environment{}, err
	}
	if len(commit) < 12 {
		return Environment{}, fmt.Errorf("%w: Commit %q zu kurz, mindestens 12 Zeichen angeben", ErrInvalidRepo, commit)
	}
	tag := commit[:12]
	if strings.TrimSpace(name) == "" {
		name = pathBase(canonical)
	}
	e := Environment{Owner: owner, Name: strings.TrimSpace(name), GitURL: canonical, Ref: strings.TrimSpace(ref), Branch: branch, Commit: commit,
		Image: fmt.Sprintf("%s/%s/%s:%s", s.cfg.Registry, s.cfg.EnvsProject, slug, tag)}
	if err := s.startBuildIfMissing(ctx, &e, slug, tag, false); err != nil {
		return Environment{}, err
	}
	return s.store.Save(ctx, e)
}

func pathBase(u string) string { return u[strings.LastIndex(u, "/")+1:] }

func jobName(slug, tag string) string {
	s := slug
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return "env-" + s + "-" + tag
}

// startBuildIfMissing sets e.Status: ready if the image exists, else creates
// (or joins) the build job. force rebuilds even an existing image.
func (s *Service) startBuildIfMissing(ctx context.Context, e *Environment, slug, tag string, force bool) error {
	if !force {
		ok, err := s.images.exists(ctx, s.cfg.EnvsProject, slug, tag)
		if err != nil {
			return err
		}
		if ok {
			e.Status, e.JobName, e.Message = StatusReady, "", ""
			return nil
		}
	}
	e.JobName = jobName(slug, tag)
	if force {
		if err := s.builds.deleteJob(ctx, s.cfg.BuildNamespace, e.JobName); err != nil {
			return err
		}
	}
	builder := s.cfg.BuilderImage
	if builder == "" {
		b, err := s.images.newestBuilder(ctx)
		if err != nil {
			return err
		}
		builder = b
	}
	job := buildJob(s.cfg, e.JobName, builder, e.GitURL, e.Commit, e.Branch, e.Image, slug)
	err := s.builds.createJob(ctx, s.cfg.BuildNamespace, job)
	if errors.Is(err, errAlreadyExists) {
		// Someone else builds the same repository and commit: share the job.
		err = nil
	}
	if err != nil {
		return err
	}
	e.Status, e.Message = StatusBuilding, ""
	return nil
}

// refresh updates a building environment from its job.
func (s *Service) refresh(ctx context.Context, e Environment) Environment {
	if e.Status != StatusBuilding || e.JobName == "" {
		return e
	}
	st, err := s.builds.jobStatus(ctx, s.cfg.BuildNamespace, e.JobName)
	switch {
	case errors.Is(err, ErrNotFound):
		// Job gone (cleaned up after a day): decide by the registry.
		if ok, rerr := s.images.exists(ctx, s.cfg.EnvsProject, slugOf(e.Image), tagOf(e.Image)); rerr == nil && ok {
			e.Status = StatusReady
		} else if rerr == nil {
			e.Status, e.Message = StatusFailed, "Build nicht mehr vorhanden, Image fehlt."
		}
	case err != nil:
		s.log.Warnw("build status", "job", e.JobName, "error", err)
		return e
	case st.Succeeded > 0:
		e.Status, e.Message = StatusReady, ""
	case st.Failed > 0:
		e.Status, e.Message = StatusFailed, "Build fehlgeschlagen, Details im Build-Log."
		for _, c := range st.Conditions {
			if c.Type == "Failed" && c.Reason == "DeadlineExceeded" {
				e.Message = "Build hat das Zeitlimit überschritten."
			}
		}
	default:
		return e
	}
	saved, err := s.store.Save(ctx, e)
	if err != nil {
		s.log.Warnw("save environment", "id", e.ID, "error", err)
		return e
	}
	return saved
}

func slugOf(image string) string {
	repo := image[strings.LastIndex(image, "/")+1:]
	return repo[:strings.Index(repo, ":")]
}

func tagOf(image string) string { return image[strings.LastIndex(image, ":")+1:] }

// List returns owner's environments (all for owner ""), with fresh build status.
func (s *Service) List(ctx context.Context, owner string) ([]Environment, error) {
	envs, err := s.store.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	for i := range envs {
		envs[i] = s.refresh(ctx, envs[i])
	}
	return envs, nil
}

// Get returns one environment; admin may read every environment.
func (s *Service) Get(ctx context.Context, id uint, caller string, admin bool) (Environment, error) {
	e, err := s.store.Get(ctx, id)
	if err != nil {
		return Environment{}, err
	}
	if e.Owner != caller && !admin {
		return Environment{}, ErrForbidden
	}
	return s.refresh(ctx, e), nil
}

// Log returns the tail of the build log.
func (s *Service) Log(ctx context.Context, e Environment) (string, error) {
	if e.JobName == "" {
		return "", ErrNotFound
	}
	return s.builds.jobLog(ctx, s.cfg.BuildNamespace, e.JobName, 400)
}

// Rebuild builds the image again (after a failure, or forced).
func (s *Service) Rebuild(ctx context.Context, e Environment) (Environment, error) {
	if err := s.startBuildIfMissing(ctx, &e, slugOf(e.Image), tagOf(e.Image), true); err != nil {
		return Environment{}, err
	}
	return s.store.Save(ctx, e)
}

// Delete forgets the environment (the image stays until Harbor's retention removes it).
func (s *Service) Delete(ctx context.Context, e Environment) error { return s.store.Delete(ctx, e.ID) }

// ServerName is the JupyterHub server name for an environment: short, stable.
func ServerName(e Environment) string {
	n := slugUnsafe.ReplaceAllString(strings.ToLower(e.Name), "-")
	n = strings.Trim(n, "-")
	if len(n) > 30 {
		n = strings.TrimRight(n[:30], "-")
	}
	if n == "" {
		n = "env"
	}
	return fmt.Sprintf("%s-%d", n, e.ID)
}

// Start starts (or reuses) the person's named server for the environment.
// gpu selects the GPU profile; JupyterHub enforces the GPU quota and answers
// with a message if it is exhausted.
func (s *Service) Start(ctx context.Context, e Environment, user string, gpu bool) (string, error) {
	e = s.refresh(ctx, e)
	if e.Status != StatusReady {
		return "", ErrNotReady
	}
	if err := s.servers.ensureUser(ctx, user); err != nil {
		return "", err
	}
	profile := "git-cpu"
	if gpu {
		profile = "git-gpu"
	}
	name := ServerName(e)
	err := s.servers.startServer(ctx, user, name, map[string]any{"profile": profile, "image--unlisted-choice": e.Image})
	var he *HubError
	if errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(he.Message, "already running") {
		err = nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/user/%s/%s/", strings.TrimRight(s.cfg.JupyterURL, "/"), url.PathEscape(user), name), nil
}

// Server is a person's JupyterHub server as shown in the UI.
type Server struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Pending string `json:"pending,omitempty"`
	URL     string `json:"url"`
	Profile string `json:"profile,omitempty"`
	Image   string `json:"image,omitempty"`
}

// Servers lists the person's servers.
func (s *Service) Servers(ctx context.Context, user string) ([]Server, error) {
	u, err := s.servers.user(ctx, user)
	if errors.Is(err, ErrNotFound) {
		return []Server{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Server{}
	for name, sv := range u.Servers {
		srv := Server{Name: name, Ready: sv.Ready, Pending: sv.Pending, URL: strings.TrimRight(s.cfg.JupyterURL, "/") + sv.URL}
		if p, ok := sv.UserOptions["profile"].(string); ok {
			srv.Profile = p
		}
		if img, ok := sv.UserOptions["image--unlisted-choice"].(string); ok {
			srv.Image = img
		}
		out = append(out, srv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// StopServer stops (and removes) one of the person's named servers.
func (s *Service) StopServer(ctx context.Context, user, name string) error {
	return s.servers.stopServer(ctx, user, name, name != "")
}

// NewForTest builds a service without cluster connections (tiers only), for tests of other packages.
func NewForTest(cfg Config) (*Service, error) {
	return newService(cfg, NewMemoryStore(), nil, nil, nil, nil, nil), nil
}
