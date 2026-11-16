package workspace

// Areas in the workspace (docs/todo.md "Bereiche im Workspace", areas_project and areas_groups): everything the platform mounts into a person's pods lies below ~/dhbw, every area a folder of the project's volume ws-<slug> mounted with subPath. They count against the project's storage like the homes, and they are kept apart by mounting, not by file permissions (every pod runs as UID 1000): a pod gets only the folders the person may see.

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
)

// The areas below the home (AreasDir): the project's shared folder at ProjectArea (in the volume still project-shared, as before 16.11.2026 at ~/project-shared), the groups at GroupsArea/<name> (in the volume groups/<id>).
const (
	AreasDir       = "dhbw"
	ProjectArea    = AreasDir + "/project"
	GroupsArea     = AreasDir + "/groups"
	ProjectAreaDir = "project-shared"
	GroupsDir      = "groups"
	// MaxGroups bounds the groups of a project: every one is a mount in each pod of its members and managers.
	MaxGroups = 200
)

// Group is a group of a project's members with its own folder (~/dhbw/groups/<name>), kept by the project's managers. ID names its folder in the volume and never changes, so renaming moves nothing; Members are role-provider tokens (user:<e-mail>, group:<group>) of people who are members of the project; ReadOnly gives them the folder only to read (material for one group). Archived: the members lose the folder, the managers keep reading it, until the project ends or the group is removed for good.
type Group struct {
	ID       string   `json:"id" example:"3f9a1c2e"`
	Name     string   `json:"name" example:"gruppe-1"`
	Members  []string `json:"members" example:"user:a.b@dhbw.de"`
	ReadOnly bool     `json:"read_only,omitempty"`
	Archived bool     `json:"archived,omitempty"`
}

// Area is a folder of the project's volume as a person's pods mount it: Mount relative to the home (below AreasDir), Dir the subPath in the volume.
type Area struct {
	Mount    string `json:"mount" example:"dhbw/groups/gruppe-1"`
	Dir      string `json:"dir" example:"groups/3f9a1c2e"`
	ReadOnly bool   `json:"read_only"`
}

var (
	groupNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,38}[a-z0-9])?$`)
	groupIDRE   = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

func newGroupID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validateGroups cleans the groups and checks them: a name as a folder name, unique (also against the archived ones, which keep their folder), members as tokens; a new group (no ID) gets one, an ID must look like one and be unique.
func validateGroups(p *Project) error {
	if len(p.Groups) > MaxGroups {
		return invalid("Höchstens %d Gruppen je Projekt.", "At most %d groups per project.", MaxGroups)
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for i := range p.Groups {
		g := &p.Groups[i]
		g.Name = strings.ToLower(strings.TrimSpace(g.Name))
		if !groupNameRE.MatchString(g.Name) {
			return invalid("Gruppenname %q ungültig: Kleinbuchstaben, Ziffern, Punkt, Binde- und Unterstrich, höchstens 40 Zeichen (er wird zum Ordnernamen).", "Invalid group name %q: lowercase letters, digits, dot, hyphen and underscore, at most 40 characters (it becomes the folder's name).", g.Name)
		}
		if names[g.Name] {
			return invalid("Gruppenname %q kommt doppelt vor.", "Group name %q appears twice.", g.Name)
		}
		names[g.Name] = true
		g.ID = strings.TrimSpace(g.ID)
		if g.ID == "" {
			g.ID = newGroupID()
			for ids[g.ID] {
				g.ID = newGroupID()
			}
		}
		if !groupIDRE.MatchString(g.ID) || ids[g.ID] {
			return invalid("Gruppe %q: Kennung %q ungültig oder doppelt.", "Group %q: id %q invalid or duplicate.", g.Name, g.ID)
		}
		ids[g.ID] = true
		members := []string{}
		for _, t := range g.Members {
			t = strings.TrimSpace(t)
			if strings.HasPrefix(t, "user:") {
				t = strings.ToLower(t)
			}
			if !strings.HasPrefix(t, "user:") && !strings.HasPrefix(t, "group:") || len(t) <= len("user:") {
				return invalid("Gruppe %q, Mitglied %q: user:<E-Mail> oder group:<Gruppe>.", "Group %q, member %q: user:<e-mail> or group:<group>.", g.Name, t)
			}
			if !slices.Contains(members, t) {
				members = append(members, t)
			}
		}
		g.Members = members
	}
	return nil
}

// inGroup tells whether one of the person's tokens is a member of the group (case-insensitive for user tokens, as e-mail addresses are).
func (g Group) inGroup(tokens []string) bool {
	for _, t := range tokens {
		for _, m := range g.Members {
			if t == m || (strings.HasPrefix(m, "user:") && strings.EqualFold(t, m)) {
				return true
			}
		}
	}
	return false
}

// AreasFor are the folders a person with the tokens and role (RoleOf, or "admin") mounts in the project: the shared folder (read-only for readers), then each group the person belongs to (read-only for readers and in a read-only group), and for the managers (and admins) every group, writable so they can put material there, an archived one only to read. Members lose an archived group. A project without storage has no areas: its pods have no volume.
func (p Project) AreasFor(tokens []string, role string) []Area {
	if p.StorageGB <= 0 || role == "" {
		return nil
	}
	out := []Area{{Mount: ProjectArea, Dir: ProjectAreaDir, ReadOnly: role == RoleReader}}
	manager := role == RoleManager || role == "admin"
	for _, g := range p.Groups {
		a := Area{Mount: GroupsArea + "/" + g.Name, Dir: GroupsDir + "/" + g.ID}
		switch {
		case manager:
			a.ReadOnly = g.Archived
		case !g.Archived && g.inGroup(tokens):
			a.ReadOnly = g.ReadOnly || role == RoleReader
		default:
			continue
		}
		out = append(out, a)
	}
	return out
}
