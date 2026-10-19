package workspace

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/llm-management-api/internal/promtext"
)

// Profile is one entry of JupyterHub's profile list, as far as projects need it. GPU: its servers or kernels use a GPU, of the class chosen at the start.
type Profile struct {
	Slug string `json:"slug" example:"gpu"`
	Name string `json:"name" example:"GPU"`
	GPU  bool   `json:"gpu,omitempty"`
	// Runtime limits JupyterHub enforces (same inventory values): the server ends after IdleMinutes without activity, after MaxHours at the latest; 0 when not configured. Since the release times (release.go) they are the defaults where neither budget nor project sets one.
	IdleMinutes int `json:"idle_minutes,omitempty" example:"30"`
	MaxHours    int `json:"max_hours,omitempty" example:"4"`
}

// Config is the catalogue (from the inventory, next to the JupyterHub profiles; the GPU classes from the GPU administration). Projects come only from the budget tree (/v1/tree).
type Config struct {
	Profiles   []Profile  `json:"profiles"`
	GPUClasses []GPUClass `json:"gpuClasses"`
	Bases      []Base     `json:"bases"`
}

// GPUClass is a combination of pool and tier of the GPU administration (docs/bauplan.md 10.13): what the tree grants (ws-gpus-<id>, ws-gpu-hours-<id>) and what a start chooses.
type GPUClass struct {
	ID       string   `json:"id" example:"general-medium"`
	Name     string   `json:"name" example:"General · medium"`
	Pool     string   `json:"pool" example:"general"`
	PoolName string   `json:"pool_name" example:"General"`
	Tier     string   `json:"tier" example:"medium"`
	TierName string   `json:"tier_name" example:"medium"`
	Nodes    int      `json:"nodes"`
	GPUs     float64  `json:"gpus"`               // GPUs of its ready nodes right now
	Archs    []string `json:"archs,omitempty"`    // architectures of its nodes (amd64, arm64)
	Hardware string   `json:"hardware,omitempty"` // what a GPU of the class is, e.g. "L4-3Q · 3 GB"
	Products string   `json:"products,omitempty"`
	// Replaces is the id from before pools this class took over; the tree renames its resources once.
	Replaces string `json:"replaces,omitempty"`
	// Lends: its pool lends idle GPUs to the other pools of the tier.
	Lends bool `json:"lends"`
}

// RenameGPUClass moves every project from an old class id to a new one: its classes, GPUs per class and budgets. For a renamed pool or tier; idempotent.
func (s *Service) RenameGPUClass(ctx context.Context, old, new string) (int, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list {
		changed := false
		for i, c := range p.GPUClasses {
			if c == old {
				p.GPUClasses[i], changed = new, true
			}
		}
		p.GPUClasses = slices.Compact(slices.Sorted(slices.Values(p.GPUClasses)))
		if v, ok := p.GPUsByClass[old]; ok {
			delete(p.GPUsByClass, old)
			p.GPUsByClass[new] = max(p.GPUsByClass[new], v)
			changed = true
		}
		for i := range p.Budgets {
			if p.Budgets[i].GPUClass == old {
				p.Budgets[i].GPUClass, changed = new, true
			}
		}
		if changed {
			if err := s.store.Update(ctx, &p); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// Base is one version of a base of the projects' environments (dhbw-workspace-images at its release): a project runs it, or an image built on it from Git. A project keeps its version (ImageBase is the version's id) until somebody changes it; a new version is a new entry, not a replacement (base_versions.go).
type Base struct {
	ID      string `json:"id" example:"pytorch-2.11-cu13"`
	Name    string `json:"name" example:"PyTorch 2.11 · CUDA 13"`
	Image   string `json:"image" example:"registry.gpu.services.dhbw.cloud/platform/base-pytorch:v2.0.0"`
	Default bool   `json:"default,omitempty"` // the version new projects get
	// Archs are the architectures the base is built for (images.yaml platforms); empty = amd64.
	Archs []string `json:"archs,omitempty" example:"amd64,arm64"`
	// Family groups the versions of one base (python, pytorch, tensorflow); also the id projects had before versions.
	Family  string `json:"family,omitempty" example:"pytorch"`
	Version string `json:"version,omitempty" example:"2.11-cu13"`
	// Status "deprecated": the version runs out: no new project can choose it, the projects on it keep it until ReplacedBy takes its place at EndOfLife (YYYY-MM-DD; empty = when the last project on it has ended). Empty = current.
	Status     string `json:"status,omitempty" enums:"deprecated"`
	ReplacedBy string `json:"replaced_by,omitempty" example:"pytorch-2.12-cu13"`
	EndOfLife  string `json:"end_of_life,omitempty" example:"2027-03-31"`
	// Projects and LastEnd: in the catalogue, how many active projects run the version and the latest day one of them ends (nil when one has no end).
	Projects int        `json:"projects"`
	LastEnd  *time.Time `json:"last_end,omitempty"`
}

// Deprecated tells whether the version runs out.
func (b Base) Deprecated() bool { return b.Status == "deprecated" }

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

// Refusal is a start that the project does not allow, with a reason for the person. Kind is what refused it, for the metrics (StartResults); empty for refusals outside a start.
type Refusal struct {
	Reason string
	Kind   string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error { return &Refusal{Reason: fmt.Sprintf(format, args...)} }

// refuseStart is a refusal of a start, of a kind of StartResults.
func refuseStart(kind, format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...), Kind: kind}
}

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
	inspect ImageInspector
	// imageMax and imageHint are the size limit and hint of project images (SetImageLimits); 0 = the defaults.
	imageMax, imageHint int64
	// scans reads Harbor's vulnerability scans (SetImageScans); scanRead is when the scan of a project's image was last read (key: slug and image).
	scans    ImageScanReader
	scanHost string
	scanRead sync.Map
	// unpullable are finished project images whose registry did not answer at the last check; the prepull leaves them out, so one broken image does not hold up the others.
	unpullable sync.Map
	// materialOK is when the material of a repository and branch was last fetchable (checkMaterial).
	materialOK sync.Map
	builds     ImageBuilder
	classes    func(ctx context.Context) ([]GPUClass, error)
	// Credentials (credentials.go): their store, the cipher of the tokens and where the projects' secrets go; nil = none.
	creds       CredentialStore
	aead        cipher.AEAD
	credSecrets func(ctx context.Context, want map[string]ProjectSecrets) error
	// starts counts the start decisions (metrics.go); in memory, back to zero on a restart.
	starts *promtext.CounterVec
}

// BudgetCheck refuses a start whose budget is used up (W4); nil = no budgets.
type BudgetCheck func(ctx context.Context, p Project, person string, gpuClass string) error

func NewService(store Store, cfg Config, running func(ctx context.Context) ([]Running, error)) *Service {
	return &Service{store: store, cfg: cfg, running: running, now: time.Now, starts: newStartCounter(cfg)}
}

// SetBudgetCheck adds the budget check to CheckStart.
func (s *Service) SetBudgetCheck(b BudgetCheck) { s.budget = b }

// SetGPUClasses sets where the GPU classes come from (the GPU administration); without it any class is accepted (no GPU cluster).
func (s *Service) SetGPUClasses(f func(ctx context.Context) ([]GPUClass, error)) { s.classes = f }

// GPUClasses are the classes of the GPU administration; nil without it or when it cannot be read.
func (s *Service) GPUClasses(ctx context.Context) []GPUClass {
	if s.classes == nil {
		return nil
	}
	out, err := s.classes(ctx)
	if err != nil {
		return nil
	}
	return out
}

// knownClass tells whether a class exists; without a GPU administration every class does.
func (s *Service) knownClass(id string) bool {
	if s.classes == nil {
		return true
	}
	return slices.ContainsFunc(s.GPUClasses(context.Background()), func(c GPUClass) bool { return c.ID == id })
}

// ClassName names a class for people ("General · medium"); the id when unknown.
func (s *Service) ClassName(ctx context.Context, id string) string {
	for _, c := range s.GPUClasses(ctx) {
		if c.ID == id {
			return c.Name
		}
	}
	return id
}

func (s *Service) Catalog(ctx context.Context) Config {
	classes := s.GPUClasses(ctx)
	if classes == nil {
		classes = []GPUClass{}
	}
	return Config{Profiles: s.cfg.Profiles, GPUClasses: classes, Bases: s.basesInUse(ctx)}
}

func (s *Service) profile(slug string) (Profile, bool) {
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
	Role         string         `json:"role" enums:"reader,member,manager,admin"`
	BudgetStatus []BudgetStatus `json:"budget_status"`
	// ImageBuild is the state of the project image's build from Git (ImageGitURL); nil = none.
	ImageBuild *ImageBuild `json:"image_build,omitempty"`
	// ReleaseCeiling is the most the project may choose as release times, per kind (every time set; max_hours 0 = no upper bound allowed); ReleaseEffective what its servers and GPU kernels enforce now (seconds).
	ReleaseCeiling   Release          `json:"release_ceiling"`
	ReleaseEffective EffectiveRelease `json:"release_effective"`
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
		if role == "" || (!all && !p.Active(s.now())) || p.NoWorkspaces {
			continue
		}
		m := Mine{Project: p, Role: role, BudgetStatus: []BudgetStatus{}, ReleaseCeiling: s.ReleaseCeiling(p), ReleaseEffective: s.EffectiveRelease(p)}
		if p.ImageGitURL != "" && s.builds != nil {
			if b, err := s.builds.ProjectImageBuild(ctx, p.Slug); err == nil {
				m.ImageBuild = b
			}
		}
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
		if roleRank[m.Role] == 0 {
			return refuse("Rolle %q unbekannt (reader, member, manager).", m.Role)
		}
		p.Members[i] = m
	}
	p.Profiles = s.knownProfiles(p.Profiles)
	// The version is stored as chosen: an empty base or the id of a family from before versions becomes the version it stands for, so a new default version changes nothing for the project.
	if len(s.cfg.Bases) > 0 {
		b, ok := s.base(p.ImageBase)
		if !ok {
			return refuse("Basis %q gibt es nicht.", p.ImageBase)
		}
		p.ImageBase = b.ID
	}
	for _, c := range p.GPUClasses {
		if !s.knownClass(c) {
			return refuse("GPU-Klasse %q gibt es nicht.", c)
		}
	}
	l := p.Limits
	if l.ServersPerPerson < 0 || l.CPUPerPerson < 0 || l.RAMGBPerPerson < 0 || l.GPUsPerPerson < 0 || p.LLMMTokens < 0 || p.LLMRPM < 0 || p.LLMTPM < 0 {
		return refuse("Grenzen dürfen nicht negativ sein.")
	}
	for c, n := range p.GPUsByClass {
		if n < 0 || !s.knownClass(c) {
			return refuse("GPUs der Klasse %q: Klasse unbekannt oder Zahl negativ.", c)
		}
	}
	// A person cannot hold more GPUs than the project has: without a GPU class that is none, so a limit set before the classes went (or by hand) would only mislead.
	if c := ProjectGPUCap(*p); c >= 0 && p.Limits.GPUsPerPerson > c {
		p.Limits.GPUsPerPerson = c
	}
	for _, b := range p.Budgets {
		if !slices.Contains([]string{"server_hours", "vcpu_hours", "ram_gb_hours", "gpu_hours"}, b.Unit) ||
			!slices.Contains([]string{"24h", "7d", "30d", "total"}, b.Window) || b.Amount < 0 {
			return refuse("Budget ungültig: Einheit server_hours, vcpu_hours, ram_gb_hours oder gpu_hours; Fenster 24h, 7d, 30d oder total; Menge nicht negativ.")
		}
	}
	return nil
}

// ProjectGPUCap is how many GPUs the project can have at the same time: the sum over its GPU classes, 0 without a class, -1 when a class has no cap of its own (gpus_by_class without an entry).
func ProjectGPUCap(p Project) int {
	total := 0
	for _, c := range p.GPUClasses {
		n, ok := p.GPUsByClass[c]
		if !ok || n == 0 {
			return -1
		}
		total += n
	}
	return total
}

func (s *Service) Get(ctx context.Context, slug string) (Project, error) {
	return s.store.Get(ctx, slug)
}

// Update replaces a project. A manager (asAdmin=false) may change only the description, the members, the environment (base, project image, its automatic updates), the material and the project's release times (within the budget's), in every project; everything else is kept. In a tree-managed project the tree's fields (keepTreeFields) stay the tree's whoever writes, so there the members do too; admins set the rest (limits, profiles, own budgets).
func (s *Service) Update(ctx context.Context, p Project, by string, asAdmin bool) (Project, error) {
	cur, err := s.store.Get(ctx, p.Slug)
	if err != nil {
		return Project{}, err
	}
	if !asAdmin {
		desc, members, base, image, gitURL, gitRef, auto, material, materialRef, release := p.Description, p.Members, p.ImageBase, p.ProjectImage, p.ImageGitURL, p.ImageGitRef, p.ImageAutoUpdate, p.MaterialGitURL, p.MaterialGitRef, p.Release
		p = cur
		p.Description, p.Members, p.ImageBase, p.ProjectImage, p.ImageGitURL, p.ImageGitRef, p.ImageAutoUpdate, p.MaterialGitURL, p.MaterialGitRef, p.Release = desc, members, base, image, gitURL, gitRef, auto, material, materialRef, release
	}
	if cur.Managed() {
		keepTreeFields(&p, cur)
	}
	if err := s.validate(&p); err != nil {
		return Project{}, err
	}
	if b, _ := s.base(p.ImageBase); b.Deprecated() && b.ID != cur.ImageBase {
		return Project{}, refuse("%s läuft aus und ist für Projekte nicht mehr wählbar; stattdessen %s.", b.Name, s.baseName(b.ReplacedBy))
	}
	if err := s.applyProjectImage(ctx, &p, cur, asAdmin); err != nil {
		return Project{}, err
	}
	if err := s.applyMaterial(ctx, &p, cur); err != nil {
		return Project{}, err
	}
	if err := withinTree(p); err != nil {
		return Project{}, err
	}
	if err := s.applyRelease(&p, cur); err != nil {
		return Project{}, err
	}
	p.UpdatedBy = by
	return p, s.store.Update(ctx, &p)
}

func (s *Service) Delete(ctx context.Context, slug string) error {
	if err := s.store.Delete(ctx, slug); err != nil {
		return err
	}
	s.forgetBuilds(ctx, slug)
	if s.creds != nil {
		list, _ := s.creds.List(ctx, slug)
		for _, c := range list {
			_ = s.creds.Delete(ctx, slug, c.Host)
		}
		if len(list) > 0 {
			_ = s.SyncCredentialSecrets(ctx)
		}
	}
	return nil
}

// StartRequest is what the hub asks before it starts a server.
type StartRequest struct {
	Person  string  `json:"user"`
	Project string  `json:"project"`
	Profile string  `json:"profile"`
	Server  string  `json:"server,omitempty"` // pod name of the server to start, so its own old pod does not count
	CPU     float64 `json:"cpu"`
	RAMGB   float64 `json:"ram_gb"`
	GPUs    float64 `json:"gpus"`
	// GPUClass is the class chosen at the start; empty = the project's only one.
	GPUClass string `json:"gpu_class,omitempty"`
	// Kernel: a GPU kernel of a running "CPU + GPU-Kernel" server (no new server, so the limit on servers does not apply).
	Kernel bool `json:"-"`
}

// CheckStart decides whether a person may start a profile in a project now. The reason of a refusal is shown in JupyterHub. Every decision is counted (aiaas_workspace_starts_total, metrics.go).
func (s *Service) CheckStart(ctx context.Context, tokens []string, r StartRequest) error {
	err := s.checkStart(ctx, tokens, r)
	s.countStart(r, err)
	return err
}

func (s *Service) checkStart(ctx context.Context, tokens []string, r StartRequest) error {
	if r.Project == "" {
		return refuseStart("project", "Kein Projekt gewählt.")
	}
	p, err := s.store.Get(ctx, r.Project)
	if errors.Is(err, ErrNotFound) {
		return refuseStart("project", "Projekt %q gibt es nicht.", r.Project)
	}
	if err != nil {
		return err
	}
	if p.RoleOf(tokens) == "" {
		return refuseStart("project", "Kein Mitglied im Projekt „%s“.", p.Name)
	}
	if !p.Active(s.now()) {
		return refuseStart("project", "Projekt „%s“ ist am %s abgelaufen.", p.Name, p.EndsAt.Format("02.01.2006"))
	}
	if p.NoWorkspaces {
		return refuseStart("project", "Projekt „%s“ hat keine Workspaces, nur LLM-Tokens.", p.Name)
	}
	if p.ImagePending() {
		return refuseStart("image_error", "Die Umgebung von Projekt „%s“ ist noch nicht gebaut. Der Bau und sein Protokoll stehen in den Projekteinstellungen (Workspaces); gestartet werden kann, sobald er fertig ist.", p.Name)
	}
	if !p.Allows(r.Profile) {
		return refuseStart("no_profile", "Workspace-Art %q ist im Projekt „%s“ nicht freigegeben.", r.Profile, p.Name)
	}
	if r.Profile == SplitProfile && !p.Derived() {
		return refuseStart("no_profile", "„CPU + GPU-Kernel“ braucht eine Umgebung auf einer unserer Basen; das Image des Projekts „%s“ ist keine. Stattdessen „CPU“ oder „GPU“ wählen.", p.Name)
	}
	class := ""
	if r.GPUs > 0 {
		if p.Limits.GPUsPerPerson == 0 || len(p.GPUClasses) == 0 {
			return refuseStart("gpu_class", "Im Projekt „%s“ sind keine GPUs freigegeben.", p.Name)
		}
		class = r.GPUClass
		if class == "" && len(p.GPUClasses) == 1 {
			class = p.GPUClasses[0]
		}
		if class == "" {
			return refuseStart("gpu_class", "Bitte eine GPU-Klasse wählen.")
		}
		if !p.AllowsGPUClass(class) {
			return refuseStart("gpu_class", "GPU-Klasse %s ist im Projekt „%s“ nicht freigegeben.", s.ClassName(ctx, class), p.Name)
		}
	}
	running, err := s.running(ctx)
	if err != nil {
		return err
	}
	var servers int
	var cpu, ram, gpus, classGPUs float64
	for _, x := range running {
		if (!r.Kernel && x.Server == r.Server) || x.Project != p.Slug {
			continue
		}
		if class != "" && x.GPUClass == class {
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
	case !r.Kernel && l.ServersPerPerson > 0 && servers >= l.ServersPerPerson:
		return refuseStart("limit", "Im Projekt „%s“ %s gleichzeitig möglich; schon %d laufen. Einen anderen Workspace zuerst beenden.", p.Name, plural(l.ServersPerPerson, "ist 1 Workspace", "sind %d Workspaces"), servers)
	case l.CPUPerPerson > 0 && cpu+r.CPU > l.CPUPerPerson+1e-9:
		return refuseStart("limit", "Im Projekt „%s“ sind %g Kerne gleichzeitig möglich; belegt sind %g, dieser Workspace bräuchte %g.", p.Name, l.CPUPerPerson, cpu, r.CPU)
	case l.RAMGBPerPerson > 0 && ram+r.RAMGB > l.RAMGBPerPerson+1e-9:
		return refuseStart("limit", "Im Projekt „%s“ sind %g GB Speicher gleichzeitig möglich; belegt sind %.1f, dieser Workspace bräuchte %.1f.", p.Name, l.RAMGBPerPerson, ram, r.RAMGB)
	case r.GPUs > 0 && gpus+r.GPUs > float64(l.GPUsPerPerson):
		return refuseStart("limit", "Im Projekt „%s“ %s gleichzeitig möglich, und %g ist schon in Benutzung. Das andere zuerst beenden.", p.Name, plural(l.GPUsPerPerson, "ist 1 GPU", "sind %d GPUs"), gpus)
	case r.GPUs > 0 && p.GPUsByClass[class] > 0 && classGPUs+r.GPUs > float64(p.GPUsByClass[class]):
		return refuseStart("gpu_busy", "Alle %d GPUs der Klasse %s im Projekt „%s“ sind gerade belegt.", p.GPUsByClass[class], s.ClassName(ctx, class), p.Name)
	}
	if !r.Kernel {
		if err := s.checkMaterial(ctx, p); err != nil {
			return err
		}
	}
	if s.budget != nil {
		err := s.budget(ctx, p, r.Person, class)
		if rf := (*Refusal)(nil); errors.As(err, &rf) && rf.Kind == "" {
			rf.Kind = "budget"
		}
		return err
	}
	return nil
}

// materialFresh is how long a successful check of a project's material counts, so not every start asks the Git server.
const materialFresh = 10 * time.Minute

// checkMaterial refuses a start whose course material cannot be fetched (repository gone, private, branch deleted, server unreachable): the server would start without it, which nobody notices until the folder is empty. A success is remembered for materialFresh.
func (s *Service) checkMaterial(ctx context.Context, p Project) error {
	if p.MaterialGitURL == "" || s.builds == nil {
		return nil
	}
	key := p.MaterialGitURL + "#" + p.MaterialGitRef
	if at, ok := s.materialOK.Load(key); ok && s.now().Sub(at.(time.Time)) < materialFresh {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if _, err := s.builds.CheckGitBranch(ctx, p.MaterialGitURL, p.MaterialGitRef, s.credentialFor(ctx, p.Slug, hostOfGit(p.MaterialGitURL))); err != nil {
		return refuseStart("material_error", "Das Material des Projekts „%s“ (%s) ist nicht abrufbar: %v. Ohne Material startet der Workspace nicht; das Repository steht in den Workspace-Einstellungen des Projekts.", p.Name, p.MaterialGitURL, err)
	}
	s.materialOK.Store(key, s.now())
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}
