package workspace

// Areas in the workspace (docs/todo.md "Bereiche im Workspace", areas_project and areas_groups): everything the platform mounts into a person's pods lies below ~/dhbw, every area a folder of the project's volume ws-<slug> mounted with subPath. They count against the project's storage like the homes, and they are kept apart by mounting, not by file permissions (every pod runs as UID 1000): a pod gets only the folders the person may see.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// The areas below the home (AreasDir): the project's shared folder at ProjectArea (in the volume project-shared), the groups at GroupsArea/<name> (in the volume groups/<id>).
const (
	AreasDir       = "dhbw"
	ProjectArea    = AreasDir + "/project"
	GroupsArea     = AreasDir + "/groups"
	ProjectAreaDir = "project-shared"
	GroupsDir      = "groups"
	// MaxGroups bounds the groups of a project: every one is a mount in each pod of its members and managers.
	MaxGroups = 200
)

// Group is a group of a project's members with its own folder (~/dhbw/groups/<name>), kept by the project's managers or, during the group choice (Project.GroupChoice), filled by the members themselves. ID names its folder in the volume and never changes, so renaming moves nothing; Members are people of the project as user:<e-mail> tokens. A group token (group:<group>) is no longer taken (decided 17.11.2026: a workspace group holds persons, a course is resolved to its people when dealing them out); one a group had before stays as long as it is not removed, and still counts its people in. ReadOnly gives them the folder only to read (material for one group). Archived: the members lose the folder, the managers keep reading it, until the project ends or the group is removed for good.
type Group struct {
	ID       string   `json:"id" example:"3f9a1c2e"`
	Name     string   `json:"name" example:"gruppe-1"`
	Members  []string `json:"members" example:"user:a.b@dhbw.de"`
	ReadOnly bool     `json:"read_only,omitempty"`
	Archived bool     `json:"archived,omitempty"`
	// StorageGB limits the group's folder (quotas.go); 0 = none, the project's volume is the bound.
	StorageGB int `json:"storage_gb,omitempty"`
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

// MaxGroupMembers bounds Project.GroupMaxMembers, the places of a group in the group choice.
const MaxGroupMembers = 1000

// validateGroups cleans the groups and checks them: a name as a folder name, unique (also against the archived ones, which keep their folder), members as tokens; a new group (no ID) gets one, an ID must look like one and be unique. Group tokens are checked against the stored project (newGroupTokens), which this does not know.
func validateGroups(p *Project) error {
	if len(p.Groups) > MaxGroups {
		return invalid("Höchstens %d Gruppen je Projekt.", "At most %d groups per project.", MaxGroups)
	}
	if p.GroupMaxMembers < 0 || p.GroupMaxMembers > MaxGroupMembers {
		return invalid("Plätze je Gruppe zwischen 0 (keine Grenze) und %d.", "Places per group between 0 (no limit) and %d.", MaxGroupMembers)
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
		if g.StorageGB < 0 || (p.StorageGB > 0 && g.StorageGB > p.StorageGB) {
			return invalid("Gruppe %q: Speichergrenze zwischen 0 und %d GB (der Speicher des Projekts).", "Group %q: storage limit between 0 and %d GB (the project's storage).", g.Name, p.StorageGB)
		}
	}
	return nil
}

// newGroupTokens refuses a group token in a group that the group did not have in the stored project cur (the same group by ID): workspace groups take persons only, but like other values only a changed one is checked, so a project with such an entry from before stays saveable and the entry keeps working until it is removed. p's groups are cleaned already (validateGroups).
func newGroupTokens(p, cur Project) error {
	before := map[string][]string{}
	for _, g := range cur.Groups {
		before[g.ID] = g.Members
	}
	for _, g := range p.Groups {
		for _, t := range g.Members {
			if strings.HasPrefix(t, "group:") && !slices.Contains(before[g.ID], t) {
				return invalid("Gruppe %q: %q ist eine Gruppe; Workspace-Gruppen nehmen nur Personen auf (die Personen eines Kurses einzeln zuordnen).", "Group %q: %q is a group; workspace groups take persons only (assign the people of a course one by one).", g.Name, t)
			}
		}
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

// AreasFor are the folders a person with the tokens and role (RoleOf, or "admin") mounts in the project: the shared folder (read-only unless the person manages the project or SharedWrite lets every member write), then each group the person belongs to (read-only in a read-only group), and for the managers (and admins) every group, writable so they can put material there, an archived one only to read. Members lose an archived group, and every group while the group choice is open (GroupChoice): otherwise somebody could join a group for a moment, start a workspace and keep reading its files after leaving. A project without storage has no areas: its pods have no volume.
func (p Project) AreasFor(tokens []string, role string) []Area {
	if p.StorageGB <= 0 || role == "" {
		return nil
	}
	manager := role == RoleManager || role == "admin"
	out := []Area{{Mount: ProjectArea, Dir: ProjectAreaDir, ReadOnly: !manager && p.SharedWrite == SharedWriteManagers}}
	for _, g := range p.Groups {
		a := Area{Mount: GroupsArea + "/" + g.Name, Dir: GroupsDir + "/" + g.ID}
		switch {
		case manager:
			a.ReadOnly = g.Archived
		case !g.Archived && !p.GroupChoice && g.inGroup(tokens):
			a.ReadOnly = g.ReadOnly
		default:
			continue
		}
		out = append(out, a)
	}
	return out
}

// GroupSeat is a group as a member sees it during the group choice: how full it is and whether it is the person's own, the names of its members only in the person's own group (who is in which other group stays the managers' business).
type GroupSeat struct {
	ID   string `json:"id" example:"3f9a1c2e"`
	Name string `json:"name" example:"gruppe-1"`
	// Count is the people in the group (user tokens; a course group from before counts nobody here), Max the places of a group (GroupMaxMembers, 0 = no limit).
	Count int  `json:"count" example:"3"`
	Max   int  `json:"max" example:"4"`
	Mine  bool `json:"mine"`
	// Members are the group's members, only in the person's own group.
	Members []string `json:"members,omitempty" example:"user:a.b@dhbw.de"`
}

// people counts the persons of a group (user tokens).
func (g Group) people() int {
	n := 0
	for _, m := range g.Members {
		if strings.HasPrefix(m, "user:") {
			n++
		}
	}
	return n
}

// GroupSeats are the open groups (not archived) of a project with an open group choice as the person with the tokens sees them; nil when the choice is closed.
func (p Project) GroupSeats(tokens []string) []GroupSeat {
	if !p.GroupChoice {
		return nil
	}
	out := []GroupSeat{}
	for _, g := range p.Groups {
		if g.Archived {
			continue
		}
		s := GroupSeat{ID: g.ID, Name: g.Name, Count: g.people(), Max: p.GroupMaxMembers, Mine: g.inGroup(tokens)}
		if s.Mine {
			s.Members = slices.Clone(g.Members)
		}
		out = append(out, s)
	}
	return out
}

// JoinGroup puts the person (their own user token, nobody else) into a group of the project during the group choice: only a member or reader of the project (by their tokens; managers assign in the settings), only into an open group with a free place (GroupMaxMembers), and only when they are in no other open group of the project, so leaving comes first. Joining the group one is in already changes nothing. Read, checked and written in one Store.Modify, so two people taking the last place one after the other get it once. A project the person has no part in is ErrNotFound, as elsewhere. The answer is the groups as the person sees them now.
func (s *Service) JoinGroup(ctx context.Context, tokens []string, person, slug, groupID string) ([]GroupSeat, error) {
	return s.changeOwnGroup(ctx, tokens, person, slug, groupID, true)
}

// LeaveGroup takes the person's own user token out of a group of the project during the group choice; the same rules as JoinGroup. Leaving a group one is not in changes nothing; one entered through a course group (a group token from before) cannot be left here, only by the managers.
func (s *Service) LeaveGroup(ctx context.Context, tokens []string, person, slug, groupID string) ([]GroupSeat, error) {
	return s.changeOwnGroup(ctx, tokens, person, slug, groupID, false)
}

func (s *Service) changeOwnGroup(ctx context.Context, tokens []string, person, slug, groupID string, join bool) ([]GroupSeat, error) {
	person = strings.ToLower(strings.TrimSpace(person))
	if person == "" {
		return nil, refuse("Ohne E-Mail-Adresse ist keine Gruppenwahl möglich.", "Choosing a group needs an e-mail address.")
	}
	me := "user:" + person
	// The person's own token is one of theirs even when the role provider did not answer (only the user token then).
	tokens = append(slices.Clone(tokens), me)
	p, err := s.store.Modify(ctx, slug, func(p *Project) error {
		switch p.RoleOf(tokens) {
		case "":
			return ErrNotFound
		case RoleManager:
			return refuse("Die Verwaltung des Projekts ordnet die Gruppen in den Einstellungen zu.", "The project's managers assign the groups in the settings.")
		}
		if !p.Active(s.now()) {
			return refuse("Projekt „%s“ ist abgelaufen.", "Project “%s” has ended.", p.Name)
		}
		if !p.GroupChoice {
			return refuse("Im Projekt „%s“ ist die Gruppenwahl gerade nicht offen; die Gruppen ordnet die Verwaltung des Projekts zu.", "Choosing a group is not open in project “%s” right now; its managers assign the groups.", p.Name)
		}
		i := slices.IndexFunc(p.Groups, func(g Group) bool { return g.ID == groupID && !g.Archived })
		if i < 0 {
			return refuse("Diese Gruppe gibt es im Projekt nicht (mehr).", "This group does not exist in the project (any more).")
		}
		g := &p.Groups[i]
		own := slices.IndexFunc(g.Members, func(m string) bool { return strings.EqualFold(m, me) })
		if !join {
			if own < 0 {
				if g.inGroup(tokens) {
					return refuse("In Gruppe „%s“ über eine Kursgruppe eingetragen; das ändert nur die Verwaltung des Projekts.", "In group “%s” through a course group; only the project's managers change that.", g.Name)
				}
				return errUnchanged
			}
			g.Members = slices.Delete(g.Members, own, own+1)
			p.UpdatedBy = person
			return nil
		}
		if g.inGroup(tokens) {
			return errUnchanged
		}
		for j, other := range p.Groups {
			if j != i && !other.Archived && other.inGroup(tokens) {
				return refuse("Schon in Gruppe „%s“; zuerst diese verlassen.", "Already in group “%s”; leave it first.", other.Name)
			}
		}
		if p.GroupMaxMembers > 0 && g.people() >= p.GroupMaxMembers {
			return refuse("Gruppe „%s“ ist voll (%d Plätze).", "Group “%s” is full (%d places).", g.Name, p.GroupMaxMembers)
		}
		g.Members = append(g.Members, me)
		p.UpdatedBy = person
		return nil
	})
	if errors.Is(err, errUnchanged) {
		p, err = s.store.Get(ctx, slug)
	}
	if err != nil {
		return nil, err
	}
	return p.GroupSeats(tokens), nil
}

// errUnchanged ends a Store.Modify that has nothing to write (joining one's own group again, leaving a group one is not in).
var errUnchanged = errors.New("unchanged")
