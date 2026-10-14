package gpu

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

var ErrInvalidRepo = errors.New("Repository nicht verwendbar")

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
	pods    podLister
	k8s     kubeDoer // kernel pods (kernels.go); nil in tests without them
	log     *zap.SugaredLogger
	// kernelCheck decides whether a person may start one more GPU kernel (SetKernelCheck).
	kernelCheck func(ctx context.Context, o *KernelOwner) error
	// whoami is the hub user of a token (kernels.go: the server's own token); cached in tokens.
	whoami func(ctx context.Context, token string) (string, error)
	tokens tokenCache
	// fleet: pools, GPU classes and node assignments (fleet.go); nil = the GPU administration is off.
	fleet    FleetStore
	fleetCfg FleetConfig
	// ready is told when a project image build is ready, so the project uses it.
	ready func(ctx context.Context, e Environment) error
	// bases are the current bases of the projects' environments at their release (SetBases).
	bases []string
}

// New connects to the GPU cluster's APIs.
func New(cfg Config, store Store, log *zap.SugaredLogger) (*Service, error) {
	k, err := newKube(cfg.KubeAPIURL, cfg.KubeToken, cfg.KubeCA)
	if err != nil {
		return nil, err
	}
	git := newGitClient()
	h := newHub(cfg.JupyterURL, cfg.JupyterAPIToken)
	s := newService(cfg, store, k, newRegistry(cfg.Registry), h,
		func(ctx context.Context, u string) (gitRefs, error) { return fetchRefs(ctx, git, u) }, log)
	s.whoami = h.whoami
	s.pods = k
	s.k8s = k
	return s, nil
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

// JupyterURL is shown to people with a project.
func (s *Service) JupyterURL() string { return s.cfg.JupyterURL }

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

// create adds a project image for owner: resolve the ref, reuse the image if it exists, otherwise start a build on base.
func (s *Service) create(ctx context.Context, owner, base, gitURL, ref, name string) (Environment, error) {
	if base == "" {
		return Environment{}, fmt.Errorf("%w: Projekt-Images aus Git sind hier nicht eingerichtet", ErrInvalidRepo)
	}
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
	e := Environment{Owner: owner, Name: strings.TrimSpace(name), GitURL: canonical, Ref: strings.TrimSpace(ref), Branch: branch, Commit: commit, Base: base}
	tag = projectImageTag(commit, e.Base)
	e.Image = fmt.Sprintf("%s/%s/%s:%s", s.cfg.Registry, s.cfg.EnvsProject, slug, tag)
	if err := s.startBuildIfMissing(ctx, &e, slug, tag, false); err != nil {
		return Environment{}, err
	}
	return s.store.Save(ctx, e)
}

func pathBase(u string) string { return u[strings.LastIndex(u, "/")+1:] }

// jobName is a Job name (at most 63 characters, no dots) for an image: the slug shortened to fit the tag.
func jobName(slug, tag string) string {
	tag = strings.ReplaceAll(tag, ".", "-")
	s := slug
	if n := 63 - len("env-") - 1 - len(tag); len(s) > min(40, n) {
		s = strings.TrimRight(s[:min(40, n)], "-")
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
	job := buildJob(s.cfg, e.JobName, builder, *e, slug)
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
		if s.ready != nil {
			if err := s.ready(ctx, e); err != nil {
				s.log.Warnw("project image: project not moved to the new build", "image", e.Image, "error", err)
				return e
			}
		}
	case st.Failed > 0:
		e.Status, e.Message = StatusFailed, "Build fehlgeschlagen, Details im Build-Log."
		// A refusal of the build script itself (lines starting with "dhbw: ", e.g. a base image that is not allowed) is the better message.
		if log, err := s.builds.jobLog(ctx, s.cfg.BuildNamespace, e.JobName, 50); err == nil {
			for _, line := range strings.Split(log, "\n") {
				if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "dhbw: "); ok {
					e.Message = msg
					break
				}
			}
		}
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
// EndServer stops a server but keeps it (named servers stay in the list and can be started again), e.g. when a budget is used up.
func (s *Service) EndServer(ctx context.Context, user, name string) error {
	return s.servers.stopServer(ctx, user, name, false)
}

func (s *Service) StopServer(ctx context.Context, user, name string) error {
	return s.servers.stopServer(ctx, user, name, name != "")
}

// NewForTest builds a service without cluster connections (memory store only, a hub without users), for tests of other packages.
func NewForTest(cfg Config) (*Service, error) {
	return newService(cfg, NewMemoryStore(), nil, nil, noHub{}, nil, nil), nil
}

type noHub struct{}

func (noHub) user(context.Context, string) (*hubUser, error)                    { return nil, ErrNotFound }
func (noHub) ensureUser(context.Context, string) error                          { return nil }
func (noHub) startServer(context.Context, string, string, map[string]any) error { return nil }
func (noHub) stopServer(context.Context, string, string, bool) error            { return nil }
