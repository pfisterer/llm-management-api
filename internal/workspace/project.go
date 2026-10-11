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
	RoleMember  = "member"
	RoleManager = "manager" // edits members, images and the description; sees the project's usage
)

// Member is a person or a group, as a role-provider token (user:a@dhbw.de, group:wwi23seb), like the access rules.
type Member struct {
	Token string `json:"token" example:"group:wwi23seb"`
	Role  string `json:"role" example:"member" enums:"member,manager"`
}

// Image is an environment stored with the project, e.g. the image of a course.
type Image struct {
	Name  string `json:"name" example:"Statistik WS26"`
	Image string `json:"image" example:"registry.gpu.services.dhbw.cloud/envs/github-com-o-statistik:0123456789ab"`
}

// Limits on concurrent use. 0 = no limit, except GPUsPerPerson where 0 = no GPU.
type Limits struct {
	ServersPerPerson int     `json:"servers_per_person" example:"2"`
	CPUPerPerson     float64 `json:"cpu_per_person" example:"4"`     // requested cores of all running servers of a person in the project
	RAMGBPerPerson   float64 `json:"ram_gb_per_person" example:"16"` // requested GiB
	GPUsPerPerson    int     `json:"gpus_per_person" example:"1"`    // GPU servers and GPU kernels together
	GPUs             int     `json:"gpus" example:"4"`               // the whole project at the same time; 0 = no limit
}

// Budget is a limit on accumulated use in a sliding window, in one unit (W4). Amount per person when PerPerson, else for the whole project.
type Budget struct {
	Unit      string  `json:"unit" example:"vcpu_hours" enums:"server_hours,vcpu_hours,ram_gb_hours,gpu_hours"`
	GPUClass  string  `json:"gpu_class,omitempty" example:"l4-3q"` // only for gpu_hours; empty = all classes
	Window    string  `json:"window" example:"30d" enums:"24h,7d,30d,total"`
	Amount    float64 `json:"amount" example:"40"`
	PerPerson bool    `json:"per_person" example:"true"`
}

// Project is what a person chooses when starting a workspace.
type Project struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	Slug        string     `json:"slug" gorm:"uniqueIndex;not null" example:"wwi23seb-ml"` // short name, also the pod label dhbw.cloud/project
	Name        string     `json:"name" example:"Machine Learning WWI23SEB"`
	Description string     `json:"description" example:"Übungen zur Vorlesung"`
	Members     []Member   `json:"members" gorm:"serializer:json"`
	Profiles    []string   `json:"profiles" gorm:"serializer:json" example:"cpu,pytorch"`
	GPUClasses  []string   `json:"gpu_classes" gorm:"serializer:json" example:"l4-3q"`
	Images      []Image    `json:"images" gorm:"serializer:json"`
	FreeImages  bool       `json:"free_images"` // W3: any image, CPU only, without home
	Limits      Limits     `json:"limits" gorm:"embedded;embeddedPrefix:limit_"`
	Budgets     []Budget   `json:"budgets" gorm:"serializer:json"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`   // after this day nobody can start in the project
	TreeNode    string     `json:"tree_node,omitempty"` // later: the node in openstack-management-api's budget tree
	CreatedBy   string     `json:"created_by,omitempty"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (Project) TableName() string { return "workspace_projects" }

// Active tells whether people can start in the project on day t.
func (p Project) Active(t time.Time) bool {
	return p.EndsAt == nil || !t.After(p.EndsAt.Add(24*time.Hour))
}

// RoleOf is the person's role in the project ("" = not a member); manager wins over member.
func (p Project) RoleOf(tokens []string) string {
	role := ""
	for _, m := range p.Members {
		for _, t := range tokens {
			if m.Token == t {
				if m.Role == RoleManager {
					return RoleManager
				}
				role = RoleMember
			}
		}
	}
	return role
}

// Allows tells whether the project lets a profile be started.
// FreeProfile is the hub profile for any image (W3); it is allowed through FreeImages, not through Profiles.
const FreeProfile = "free"

func (p Project) Allows(profile string) bool {
	if profile == FreeProfile {
		return p.FreeImages
	}
	return slices.Contains(p.Profiles, profile)
}

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
