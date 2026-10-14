package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Profile is one entry of JupyterHub's profile list, as far as projects need it. GPUClass empty = CPU only.
type Profile struct {
	Slug     string `json:"slug" example:"pytorch"`
	Name     string `json:"name" example:"PyTorch (GPU)"`
	GPUClass string `json:"gpu_class,omitempty" example:"l4-3q"`
}

// Config is the catalogue (from the inventory, next to the JupyterHub profiles). Projects come only from the budget tree (/v1/tree).
type Config struct {
	Profiles   []Profile `json:"profiles"`
	GPUClasses []string  `json:"gpuClasses"`
}

// Running is a running (or waiting) workspace pod, for the limits on concurrent use.
type Running struct {
	Server   string // pod name; a restart of the same server does not count twice
	Person   string
	Project  string
	Profile  string
	GPUClass string
	CPU      float64
	RAMGB    float64
	GPUs     float64
}

// Refusal is a start that the project does not allow, with a reason for the person.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error { return &Refusal{Reason: fmt.Sprintf(format, args...)} }

// Service is the projects' logic.
type Service struct {
	store   Store
	cfg     Config
	running func(ctx context.Context) ([]Running, error)
	budget  BudgetCheck
	usage   UsageReader
	llm     LLMUsed
	now     func() time.Time
	storage storageState
	labels  ImageLabels
}

// ImageLabels reads an image's labels from its registry, for the check of course images; nil = no check.
type ImageLabels func(ctx context.Context, image string) (map[string]string, error)

// SetImageLabels adds the check of course images (KernelImage) to Update.
func (s *Service) SetImageLabels(f ImageLabels) { s.labels = f }

// BaseLabel marks notebook-base (dhbw-workspace-images) and every image derived from it; BaseVersions are the major versions a course image may be built on.
const BaseLabel = "cloud.dhbw.base"

var BaseVersions = []string{"notebook-v1"}

var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]+$`)

// checkKernelImage refuses a course image that is not derived from notebook-base; an unchanged one is not checked again.
func (s *Service) checkKernelImage(ctx context.Context, p *Project, cur string) error {
	p.KernelImage = strings.TrimSpace(p.KernelImage)
	if p.KernelImage == "" || p.KernelImage == cur {
		return nil
	}
	if !imageRE.MatchString(p.KernelImage) {
		return refuse("Kursimage %q ist kein Image-Name wie ghcr.io/org/kurs:2026.", p.KernelImage)
	}
	if s.labels == nil {
		return nil
	}
	labels, err := s.labels(ctx, p.KernelImage)
	if err != nil {
		return refuse("Kursimage %s ist nicht abrufbar (es muss öffentlich sein): %v", p.KernelImage, err)
	}
	if !slices.Contains(BaseVersions, labels[BaseLabel]) {
		return refuse("Kursimage %s ist nicht von registry.gpu.services.dhbw.cloud/platform/notebook-base abgeleitet (Label %s fehlt). Für beliebige Images gibt es „Freies Image“.", p.KernelImage, BaseLabel)
	}
	return nil
}

// BudgetCheck refuses a start whose budget is used up (W4); nil = no budgets.
type BudgetCheck func(ctx context.Context, p Project, person string, gpuClass string) error

func NewService(store Store, cfg Config, running func(ctx context.Context) ([]Running, error)) *Service {
	return &Service{store: store, cfg: cfg, running: running, now: time.Now}
}

// SetBudgetCheck adds the budget check to CheckStart.
func (s *Service) SetBudgetCheck(b BudgetCheck) { s.budget = b }

func (s *Service) Catalog() Config {
	return Config{Profiles: s.cfg.Profiles, GPUClasses: s.cfg.GPUClasses}
}

func (s *Service) profile(slug string) (Profile, bool) {
	if slug == FreeGPUProfile && len(s.cfg.GPUClasses) > 0 {
		// Any image with a GPU runs on the hub's GPU queue, which is the first class (docs/bauplan.md 6.6).
		return Profile{Slug: FreeGPUProfile, GPUClass: s.cfg.GPUClasses[0]}, true
	}
	for _, p := range s.cfg.Profiles {
		if p.Slug == slug {
			return p, true
		}
	}
	return Profile{}, false
}

// Mine is a project with the person's role in it and its budgets as far as used (by the person for budgets per person).
type Mine struct {
	Project
	Role         string         `json:"role" enums:"member,manager,admin"`
	BudgetStatus []BudgetStatus `json:"budget_status"`
}

// ForPerson lists the active projects the person belongs to; admins (all=true) get every project, with role "admin" where they are no member.
func (s *Service) ForPerson(ctx context.Context, tokens []string, person string, all bool) ([]Mine, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []Mine{}
	for _, p := range list {
		role := p.RoleOf(tokens)
		if role == "" && all {
			role = "admin"
		}
		if role == "" || (!all && !p.Active(s.now())) {
			continue
		}
		m := Mine{Project: p, Role: role, BudgetStatus: []BudgetStatus{}}
		if person != "" {
			if st, err := s.BudgetStatus(ctx, p, person); err == nil {
				m.BudgetStatus = st
			}
		}
		out = append(out, m)
	}
	return out, nil
}

var slugRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

func (s *Service) validate(p *Project) error {
	p.Slug = strings.ToLower(strings.TrimSpace(p.Slug))
	p.Name = strings.TrimSpace(p.Name)
	if !slugRE.MatchString(p.Slug) {
		return refuse("Kurzname ungültig: Kleinbuchstaben, Ziffern und Bindestriche, höchstens 40 Zeichen.")
	}
	if p.Name == "" {
		p.Name = p.Slug
	}
	for i, m := range p.Members {
		m.Token = strings.TrimSpace(m.Token)
		if !strings.HasPrefix(m.Token, "user:") && !strings.HasPrefix(m.Token, "group:") {
			return refuse("Mitglied %q: user:<E-Mail> oder group:<Gruppe>.", m.Token)
		}
		if m.Role == "" {
			m.Role = RoleMember
		}
		if m.Role != RoleMember && m.Role != RoleManager {
			return refuse("Rolle %q unbekannt (member, manager).", m.Role)
		}
		p.Members[i] = m
	}
	for _, pr := range p.Profiles {
		if _, ok := s.profile(pr); !ok {
			return refuse("Profil %q gibt es nicht.", pr)
		}
	}
	for _, c := range p.GPUClasses {
		if !slices.Contains(s.cfg.GPUClasses, c) {
			return refuse("GPU-Klasse %q gibt es nicht.", c)
		}
	}
	for _, im := range p.Images {
		if strings.TrimSpace(im.Image) == "" {
			return refuse("Image ohne Adresse.")
		}
	}
	l := p.Limits
	if l.ServersPerPerson < 0 || l.CPUPerPerson < 0 || l.RAMGBPerPerson < 0 || l.GPUsPerPerson < 0 || p.LLMMTokens < 0 || p.LLMRPM < 0 || p.LLMTPM < 0 {
		return refuse("Grenzen dürfen nicht negativ sein.")
	}
	for c, n := range p.GPUsByClass {
		if n < 0 || !slices.Contains(s.cfg.GPUClasses, c) {
			return refuse("GPUs der Klasse %q: Klasse unbekannt oder Zahl negativ.", c)
		}
	}
	for _, b := range p.Budgets {
		if !slices.Contains([]string{"server_hours", "vcpu_hours", "ram_gb_hours", "gpu_hours"}, b.Unit) ||
			!slices.Contains([]string{"24h", "7d", "30d", "total"}, b.Window) || b.Amount < 0 {
			return refuse("Budget ungültig: Einheit server_hours, vcpu_hours, ram_gb_hours oder gpu_hours; Fenster 24h, 7d, 30d oder total; Menge nicht negativ.")
		}
	}
	return nil
}

func (s *Service) Get(ctx context.Context, slug string) (Project, error) {
	return s.store.Get(ctx, slug)
}

// Update replaces a project. A manager (asAdmin=false) may change only the description, the members, the images and the course image; everything else is kept.
func (s *Service) Update(ctx context.Context, p Project, by string, asAdmin bool) (Project, error) {
	cur, err := s.store.Get(ctx, p.Slug)
	if err != nil {
		return Project{}, err
	}
	switch {
	case cur.Managed():
		// The tree owns members, end and the project's amounts; managers and
		// admins set how the project uses them.
		keepTreeFields(&p, cur)
	case !asAdmin:
		desc, members, images, kernelImage := p.Description, p.Members, p.Images, p.KernelImage
		p = cur
		p.Description, p.Members, p.Images, p.KernelImage = desc, members, images, kernelImage
	}
	if err := s.validate(&p); err != nil {
		return Project{}, err
	}
	if err := s.checkKernelImage(ctx, &p, cur.KernelImage); err != nil {
		return Project{}, err
	}
	if err := withinTree(p); err != nil {
		return Project{}, err
	}
	p.UpdatedBy = by
	return p, s.store.Update(ctx, &p)
}

func (s *Service) Delete(ctx context.Context, slug string) error { return s.store.Delete(ctx, slug) }

// StartRequest is what the hub asks before it starts a server.
type StartRequest struct {
	Person  string  `json:"user"`
	Project string  `json:"project"`
	Profile string  `json:"profile"`
	Server  string  `json:"server,omitempty"` // pod name of the server to start, so its own old pod does not count
	CPU     float64 `json:"cpu"`
	RAMGB   float64 `json:"ram_gb"`
	GPUs    float64 `json:"gpus"`
}

// CheckStart decides whether a person may start a profile in a project now. The reason of a refusal is shown in JupyterHub.
func (s *Service) CheckStart(ctx context.Context, tokens []string, r StartRequest) error {
	if r.Project == "" {
		return refuse("Kein Projekt gewählt.")
	}
	p, err := s.store.Get(ctx, r.Project)
	if errors.Is(err, ErrNotFound) {
		return refuse("Projekt %q gibt es nicht.", r.Project)
	}
	if err != nil {
		return err
	}
	if p.RoleOf(tokens) == "" {
		return refuse("Kein Mitglied im Projekt „%s“.", p.Name)
	}
	if !p.Active(s.now()) {
		return refuse("Projekt „%s“ ist am %s abgelaufen.", p.Name, p.EndsAt.Format("02.01.2006"))
	}
	if !p.Allows(r.Profile) {
		return refuse("Profil %q ist im Projekt „%s“ nicht freigegeben.", r.Profile, p.Name)
	}
	pr, _ := s.profile(r.Profile)
	if r.GPUs > 0 {
		if p.Limits.GPUsPerPerson == 0 {
			return refuse("Im Projekt „%s“ sind keine GPUs freigegeben.", p.Name)
		}
		if pr.GPUClass != "" && !p.AllowsGPUClass(pr.GPUClass) {
			return refuse("GPU-Klasse %s ist im Projekt „%s“ nicht freigegeben.", pr.GPUClass, p.Name)
		}
	}
	running, err := s.running(ctx)
	if err != nil {
		return err
	}
	var servers int
	var cpu, ram, gpus, classGPUs float64
	for _, x := range running {
		if x.Server == r.Server || x.Project != p.Slug {
			continue
		}
		if x.GPUClass == pr.GPUClass {
			classGPUs += x.GPUs
		}
		if !strings.EqualFold(x.Person, r.Person) {
			continue
		}
		if x.Profile != "gpu-kernel" {
			servers++
		}
		cpu += x.CPU
		ram += x.RAMGB
		gpus += x.GPUs
	}
	l := p.Limits
	switch {
	case l.ServersPerPerson > 0 && servers >= l.ServersPerPerson:
		return refuse("Im Projekt „%s“ %s gleichzeitig möglich; schon %d laufen. Einen anderen Server zuerst beenden.", p.Name, plural(l.ServersPerPerson, "ist 1 Server", "sind %d Server"), servers)
	case l.CPUPerPerson > 0 && cpu+r.CPU > l.CPUPerPerson+1e-9:
		return refuse("Im Projekt „%s“ sind %g Kerne gleichzeitig möglich; belegt sind %g, dieser Server bräuchte %g.", p.Name, l.CPUPerPerson, cpu, r.CPU)
	case l.RAMGBPerPerson > 0 && ram+r.RAMGB > l.RAMGBPerPerson+1e-9:
		return refuse("Im Projekt „%s“ sind %g GB Speicher gleichzeitig möglich; belegt sind %.1f, dieser Server bräuchte %.1f.", p.Name, l.RAMGBPerPerson, ram, r.RAMGB)
	case r.GPUs > 0 && gpus+r.GPUs > float64(l.GPUsPerPerson):
		return refuse("Im Projekt „%s“ %s gleichzeitig möglich, und %g ist schon in Benutzung. Das andere zuerst beenden.", p.Name, plural(l.GPUsPerPerson, "ist 1 GPU", "sind %d GPUs"), gpus)
	case r.GPUs > 0 && p.GPUsByClass[pr.GPUClass] > 0 && classGPUs+r.GPUs > float64(p.GPUsByClass[pr.GPUClass]):
		return refuse("Alle %d GPUs der Klasse %s im Projekt „%s“ sind gerade belegt.", p.GPUsByClass[pr.GPUClass], pr.GPUClass, p.Name)
	}
	if s.budget != nil {
		return s.budget(ctx, p, r.Person, pr.GPUClass)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

// KernelImages are the course images of the projects people can start in now, for pulling them onto the nodes ahead of time.
func (s *Service) KernelImages(ctx context.Context) ([]string, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range list {
		if p.KernelImage != "" && p.Active(s.now()) && !slices.Contains(out, p.KernelImage) {
			out = append(out, p.KernelImage)
		}
	}
	return out, nil
}

// ReplaceKernelImage moves every project from a course image to its rebuild on a new base (gpu.Service.RebaseCourseImages); no check again, the rebuild has the label by construction.
func (s *Service) ReplaceKernelImage(ctx context.Context, old, new string) error {
	list, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range list {
		if p.KernelImage != old {
			continue
		}
		p.KernelImage, p.UpdatedBy = new, "course-image-rebuild"
		if err := s.store.Update(ctx, &p); err != nil {
			return err
		}
	}
	return nil
}
