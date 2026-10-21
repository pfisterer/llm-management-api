// Package workspace holds the projects through which people use workspaces (Jupyter servers, later Coder), modelled on openstack-management-api's projects: members, what may be started (profiles, GPU classes, images), limits for concurrent use and budgets per window (docs/bauplan.md, Stufe 4). Until projects come from the budget tree (BudgetSource, resource-management-api), admins manage them here.
package workspace

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Roles in a project.
const (
	RoleReader  = "reader"  // uses the project; its shared folder (project-shared) only reads
	RoleMember  = "member"  // uses the project and writes its shared folder
	RoleManager = "manager" // edits members, images and the description; sees the project's usage
)

// roleRank orders the roles: a person with several matching tokens gets the highest.
var roleRank = map[string]int{RoleReader: 1, RoleMember: 2, RoleManager: 3}

// Member is a person or a group, as a role-provider token (user:a@dhbw.de, group:wwi23seb).
type Member struct {
	Token string `json:"token" example:"group:wwi23seb"`
	Role  string `json:"role" example:"member" enums:"reader,member,manager"`
}

// Limits on concurrent use. 0 = no limit, except GPUsPerPerson where 0 = no GPU.
type Limits struct {
	ServersPerPerson int     `json:"servers_per_person" example:"2"`
	CPUPerPerson     float64 `json:"cpu_per_person" example:"4"`     // requested cores of all running servers of a person in the project
	RAMGBPerPerson   float64 `json:"ram_gb_per_person" example:"16"` // requested GiB
	GPUsPerPerson    int     `json:"gpus_per_person" example:"1"`    // GPU servers and GPU kernels together
}

// Budget is a limit on accumulated use in a window, in one unit (W4). Amount per person when PerPerson, else for the whole project. Tree marks a budget the budget tree set: the project's share of a budget there, which only the tree changes; the others are the managers' (shares per person, shorter windows).
type Budget struct {
	Unit      string  `json:"unit" example:"vcpu_hours" enums:"server_hours,vcpu_hours,ram_gb_hours,gpu_hours"`
	GPUClass  string  `json:"gpu_class,omitempty" example:"l4-3q"` // only for gpu_hours; empty = all classes
	Window    string  `json:"window" example:"30d" enums:"24h,7d,30d,total"`
	Amount    float64 `json:"amount" example:"40"`
	PerPerson bool    `json:"per_person" example:"true"`
	Tree      bool    `json:"tree,omitempty"`
}

// Project is what a person chooses when starting a workspace.
type Project struct {
	ID          uint     `json:"id" gorm:"primaryKey"`
	Slug        string   `json:"slug" gorm:"uniqueIndex;not null" example:"wwi23seb-ml"` // short name, also the pod label dhbw.cloud/project
	Name        string   `json:"name" example:"Machine Learning WWI23SEB"`
	Description string   `json:"description" example:"Übungen zur Vorlesung"`
	Members     []Member `json:"members" gorm:"serializer:json"`
	Profiles    []string `json:"profiles" gorm:"serializer:json" example:"cpu,pytorch"`
	GPUClasses  []string `json:"gpu_classes" gorm:"serializer:json" example:"l4-3q"`
	// The project's environment, run by every profile but the free images (docs/bauplan.md 10.12; Service.EffectiveImage): ImageBase is the chosen base (Config.Bases, empty = the default). ProjectImage is a finished image derived from a base (label BaseLabel), or, with ImageGitURL, the last successful build from that repository on ImageBase; empty = the base itself.
	ImageBase    string `json:"image_base,omitempty" example:"pytorch"`
	ProjectImage string `json:"project_image,omitempty" example:"ghcr.io/dozent/ml-kurs:2026"`
	// ImageGitURL builds ProjectImage from a public repository (requirements.txt, environment.yml, apt.txt, postBuild) on ImageBase, rebuilt on its new releases; ImageGitRef is a branch, tag or commit (empty = default branch). Only the build's source: its files reach the servers as material only when MaterialGitURL names it too.
	ImageGitURL string `json:"image_git_url,omitempty" example:"https://github.com/dozent/ml-kurs"`
	ImageGitRef string `json:"image_git_ref,omitempty" example:"main"`
	// MaterialGitURL is the project's course material, whatever its environment: a public repository every server clones into ~/projects/<repo> at its start and later pulls without overwriting the person's own changes (the hub's git-checkout init container); MaterialGitRef is its branch (empty = default branch).
	MaterialGitURL string `json:"material_git_url,omitempty" example:"https://github.com/dozent/ml-kurs"`
	MaterialGitRef string `json:"material_git_ref,omitempty" example:"main"`
	// ImageDerived tells whether an existing ProjectImage is derived from a base (label BaseLabel of the image at ImageDigest). Any image may be the project's; one that is not derived runs as it is (its own jupyterhub-singleuser), without the GPU kernel (Derived).
	ImageDerived bool `json:"image_derived,omitempty"`
	// ImageDigest is what an existing ProjectImage's tag pointed at when it was taken: servers run exactly this image (EffectiveImage), so a moved tag (latest) changes nothing unnoticed and every node runs the same. ImageUpdate is a newer digest the tag points at now (CheckProjectImages), taken with UpdateProjectImage or, with ImageAutoUpdate, by the check itself.
	ImageDigest     string `json:"image_digest,omitempty" example:"sha256:4f2a…"`
	ImageUpdate     string `json:"image_update,omitempty" example:"sha256:9c1e…"`
	ImageAutoUpdate bool   `json:"image_auto_update,omitempty"`
	// ImageArchs are the architectures the ProjectImage has (linux: amd64, arm64), from its registry: an existing image along with ImageDigest, a build from Git once it is read (inspectBuilt; for a build for several architectures those of its index); empty = amd64.
	ImageArchs []string `json:"image_archs,omitempty" gorm:"serializer:json"`
	// ImageSize is the project image's download (sum of its compressed layers, bytes), ImageUser the user its config names (USER), ImageWarnings what the checks at saving and at every recheck found about it (imageWarnings: runs as root or another UID, HOME elsewhere, its own entrypoint, jupyterhub-singleuser not recognisable, large); they only inform, a saved image is never refused later. Also for an image built from Git, read once its build is ready.
	ImageSize     int64    `json:"image_size,omitempty"`
	ImageUser     string   `json:"image_user,omitempty"`
	ImageWarnings []string `json:"image_warnings,omitempty" gorm:"serializer:json"`
	// ImageScan is Harbor's vulnerability scan (Trivy) of the image the project runs, for images in the platform's Harbor; nil = not there, not scanned yet or no scanner. It never blocks anything.
	ImageScan *ImageScan `json:"image_scan,omitempty" gorm:"serializer:json"`
	Limits    Limits     `json:"limits" gorm:"embedded;embeddedPrefix:limit_"`
	Budgets   []Budget   `json:"budgets" gorm:"serializer:json"`
	// Release are the project's release times (release.go): its choice within ReleaseMax, empty = the budget's; managers and admins set them. ReleaseMax are the budget's, the most the project may choose; empty = today's times of the kinds of workspace. TreeRelease: the budget tree sends ReleaseMax, then nobody changes it here; before, admins set it.
	Release     Release `json:"release" gorm:"column:release_times;serializer:json"`
	ReleaseMax  Release `json:"release_max" gorm:"column:release_max;serializer:json"`
	TreeRelease bool    `json:"tree_release,omitempty" gorm:"column:tree_release"`
	// ScratchGB is the most local storage (GB) its servers and GPU kernels may write outside the home (scratch.go): from the budget tree, which then owns it; for a project without the tree its admins set it. 0 = the configured default.
	ScratchGB int `json:"scratch_gb" gorm:"column:scratch_gb" example:"5"`
	// GPUsByClass is how many GPUs of a class the whole project may use at the same time; a class without an entry has no such limit.
	GPUsByClass map[string]int `json:"gpus_by_class" gorm:"column:gpus_by_class;serializer:json"`
	// LLMMTokens is the project's LLM tokens per 30 days in millions, from the budget tree: one pool (a LiteLLM team) that all members draw from together, not an amount per member.
	LLMMTokens int `json:"llm_mtokens" gorm:"column:llm_mtokens"`
	// LLMRPM and LLMTPM are the project's requests and tokens per minute for all members together, from the budget tree; 0 = the configured default.
	LLMRPM int `json:"llm_rpm" gorm:"column:llm_rpm"`
	LLMTPM int `json:"llm_tpm" gorm:"column:llm_tpm"`
	// LLMModels are model groups the tree granted the project on top of the base models.
	LLMModels []string `json:"llm_models" gorm:"column:llm_models;serializer:json"`
	// StorageGB is the project's workspace storage in GB, from the budget tree: one volume for the homes of its members and their shared folder (StoragePVCPrefix); 0 = no volume, servers then start with an empty home that is lost when they end.
	StorageGB int `json:"storage_gb" gorm:"column:storage_gb"`
	// DeleteAt is the day the budget tree deletes the project with its workspace storage (its end plus a grace period); nil = not planned.
	DeleteAt *time.Time `json:"delete_at,omitempty" gorm:"column:delete_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"` // after this day nobody can start in the project
	// NoWorkspaces: the tree gives the project no workspace resources (LLM tokens only, docs/bauplan.md 8.14); it is then not offered for servers.
	NoWorkspaces bool `json:"no_workspaces,omitempty" gorm:"column:no_workspaces"`
	// TreeProfiles: the budget tree grants the server types (Profiles); they are then the tree's like its other fields.
	TreeProfiles bool      `json:"tree_profiles,omitempty" gorm:"column:tree_profiles"`
	TreeNode     string    `json:"tree_node,omitempty"` // the project's node in the budget tree (resource-management-api); set = the tree manages members, end, the project's budgets and GPUs, and the LLM tokens, rate limits and models
	CreatedBy    string    `json:"created_by,omitempty"`
	UpdatedBy    string    `json:"updated_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (Project) TableName() string { return "workspace_projects" }

// ImagePending tells whether the project's environment is built from Git and has no finished image yet (building or failed): its servers would start in the bare base, so starts wait for the build.
func (p Project) ImagePending() bool { return p.ImageGitURL != "" && p.ProjectImage == "" }

// Managed tells whether the budget tree manages the project.
func (p Project) Managed() bool { return p.TreeNode != "" }

// Active tells whether people can start in the project on day t.
func (p Project) Active(t time.Time) bool {
	return p.EndsAt == nil || !t.After(p.EndsAt.Add(24*time.Hour))
}

// RoleOf is the person's role in the project ("" = not a member): the highest of the tokens that match (manager over member over reader).
func (p Project) RoleOf(tokens []string) string {
	role := ""
	for _, m := range p.Members {
		for _, t := range tokens {
			r := m.Role
			if r == "" {
				r = RoleMember
			}
			if m.Token == t && roleRank[r] > roleRank[role] {
				role = r
			}
		}
	}
	return role
}

// SplitProfile is "CPU + GPU-Kernel": the notebook on a CPU node, GPU kernels in their own pods from the same image, which only an image derived from a base can do (Derived).
const SplitProfile = "cpu-gpukernel"

// Allows tells whether the project lets a profile be started.
func (p Project) Allows(profile string) bool { return slices.Contains(p.Profiles, profile) }

// Derived tells whether the project's environment is a base or built on one — with JupyterLab, VS Code and the GPU kernel —, rather than an image that runs as it is.
func (p Project) Derived() bool { return p.ImageGitURL != "" || p.ProjectImage == "" || p.ImageDerived }

// AllowsGPUClass tells whether GPUs of a class may be used.
func (p Project) AllowsGPUClass(class string) bool { return slices.Contains(p.GPUClasses, class) }

var (
	ErrNotFound = errors.New("project not found")
	ErrExists   = errors.New("project exists")
)

// Store keeps the projects.
type Store interface {
	List(ctx context.Context) ([]Project, error)
	Get(ctx context.Context, slug string) (Project, error)
	Create(ctx context.Context, p *Project) error
	Update(ctx context.Context, p *Project) error
	Delete(ctx context.Context, slug string) error
}
