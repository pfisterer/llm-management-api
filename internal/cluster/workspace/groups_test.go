package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateGroups(t *testing.T) {
	p := Project{Groups: []Group{
		{Name: " Gruppe-1 ", Members: []string{"user:A@dhbw.de", " user:a@dhbw.de", "group:wwi24"}},
		{ID: "0000aaaa", Name: "gruppe-2"},
	}}
	if err := validateGroups(&p); err != nil {
		t.Fatal(err)
	}
	g := p.Groups[0]
	if g.Name != "gruppe-1" || !groupIDRE.MatchString(g.ID) || !slices.Equal(g.Members, []string{"user:a@dhbw.de", "group:wwi24"}) {
		t.Errorf("cleaned: %+v", g)
	}
	if p.Groups[1].ID != "0000aaaa" {
		t.Errorf("kept id: %+v", p.Groups[1])
	}
	for name, groups := range map[string][]Group{
		"bad name":      {{Name: "../x"}},
		"space":         {{Name: "gruppe 1"}},
		"empty":         {{Name: ""}},
		"twice":         {{Name: "a"}, {Name: "A"}},
		"bad id":        {{ID: "../../homes", Name: "a"}},
		"id twice":      {{ID: "0000aaaa", Name: "a"}, {ID: "0000aaaa", Name: "b"}},
		"bad member":    {{Name: "a", Members: []string{"a@dhbw.de"}}},
		"empty member":  {{Name: "a", Members: []string{"user:"}}},
		"archived name": {{Name: "a", Archived: true}, {Name: "a"}},
	} {
		p := Project{Groups: groups}
		if err := validateGroups(&p); err == nil {
			t.Errorf("%s: accepted %+v", name, p.Groups)
		}
	}
	many := Project{Groups: make([]Group, MaxGroups+1)}
	for i := range many.Groups {
		many.Groups[i].Name = "g" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if err := validateGroups(&many); err == nil {
		t.Error("too many groups accepted")
	}
}

func TestAreasFor(t *testing.T) {
	p := Project{StorageGB: 10, Groups: []Group{
		{ID: "0000aaaa", Name: "g1", Members: []string{"user:a@dhbw.de"}},
		{ID: "0000bbbb", Name: "g2", Members: []string{"group:wwi24"}, ReadOnly: true},
		{ID: "0000cccc", Name: "alt", Members: []string{"user:a@dhbw.de"}, Archived: true},
	}}
	mounts := func(areas []Area) []string {
		out := []string{}
		for _, a := range areas {
			ro := "rw"
			if a.ReadOnly {
				ro = "ro"
			}
			out = append(out, a.Mount+"="+a.Dir+":"+ro)
		}
		return out
	}
	for name, c := range map[string]struct {
		tokens []string
		role   string
		want   []string
	}{
		"member of g1":         {[]string{"user:A@dhbw.de"}, RoleMember, []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw"}},
		"member via group, ro": {[]string{"user:b@dhbw.de", "group:wwi24"}, RoleMember, []string{"dhbw/project=project-shared:rw", "dhbw/groups/g2=groups/0000bbbb:ro"}},
		"reader in g1":         {[]string{"user:a@dhbw.de"}, RoleReader, []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw"}},
		"no group":             {[]string{"user:c@dhbw.de"}, RoleMember, []string{"dhbw/project=project-shared:rw"}},
		"manager sees all":     {[]string{"user:m@dhbw.de"}, RoleManager, []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw", "dhbw/groups/g2=groups/0000bbbb:rw", "dhbw/groups/alt=groups/0000cccc:ro"}},
		"admin like a manager": {nil, "admin", []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw", "dhbw/groups/g2=groups/0000bbbb:rw", "dhbw/groups/alt=groups/0000cccc:ro"}},
		"not a member":         {[]string{"user:a@dhbw.de"}, "", []string{}},
	} {
		if got := mounts(p.AreasFor(c.tokens, c.role)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	// Only the managers write the shared folder.
	p.SharedWrite = SharedWriteManagers
	if got := mounts(p.AreasFor([]string{"user:a@dhbw.de"}, RoleMember)); got[0] != "dhbw/project=project-shared:ro" {
		t.Errorf("member, managers write: %v", got)
	}
	if got := mounts(p.AreasFor([]string{"user:m@dhbw.de"}, RoleManager)); got[0] != "dhbw/project=project-shared:rw" {
		t.Errorf("manager, managers write: %v", got)
	}
	if got := (Project{Groups: p.Groups}).AreasFor([]string{"user:a@dhbw.de"}, RoleMember); got != nil {
		t.Errorf("without storage: %v", got)
	}
	// While the group choice is open, the group folders are the managers' alone; closed again, the members have theirs back.
	p.SharedWrite = SharedWriteAll
	p.GroupChoice = true
	if got := mounts(p.AreasFor([]string{"user:a@dhbw.de"}, RoleMember)); !slices.Equal(got, []string{"dhbw/project=project-shared:rw"}) {
		t.Errorf("member during the choice: %v", got)
	}
	if got := mounts(p.AreasFor([]string{"user:a@dhbw.de"}, RoleReader)); !slices.Equal(got, []string{"dhbw/project=project-shared:rw"}) {
		t.Errorf("reader during the choice: %v", got)
	}
	if got := p.AreasFor([]string{"user:m@dhbw.de"}, RoleManager); len(got) != 4 {
		t.Errorf("manager during the choice: %v", mounts(got))
	}
	p.GroupChoice = false
	if got := mounts(p.AreasFor([]string{"user:a@dhbw.de"}, RoleMember)); len(got) != 2 {
		t.Errorf("member after the choice: %v", got)
	}
}

// Workspace groups take persons: a group token is refused unless the same group had it already (a project from before stays saveable), and also moving one into another group is a new entry.
func TestGroupTokensOnlyWhereKept(t *testing.T) {
	ctx := context.Background()
	s := service()
	mgr := []Member{{Token: "user:m@dhbw.de", Role: RoleManager}, {Token: "group:wwi24"}}
	if _, err := s.create(ctx, Project{Slug: "kurs", Members: mgr, StorageGB: 10, Groups: []Group{{ID: "0000aaaa", Name: "alt", Members: []string{"group:wwi24", "user:a@dhbw.de"}}, {ID: "0000bbbb", Name: "neu"}}}, "admin"); err != nil {
		t.Fatal(err)
	}
	save := func(groups []Group) error {
		p, _ := s.Get(ctx, "kurs")
		p.Groups = groups
		_, err := s.Update(ctx, p, "m@dhbw.de", false)
		return err
	}
	if err := save([]Group{{ID: "0000aaaa", Name: "alt-umbenannt", Members: []string{"group:wwi24", "user:a@dhbw.de", "user:b@dhbw.de"}}, {ID: "0000bbbb", Name: "neu"}}); err != nil {
		t.Fatalf("kept group token: %v", err)
	}
	for name, groups := range map[string][]Group{
		"another token":         {{ID: "0000aaaa", Name: "alt", Members: []string{"group:wwi24", "group:wwi25"}}, {ID: "0000bbbb", Name: "neu"}},
		"moved to another":      {{ID: "0000aaaa", Name: "alt"}, {ID: "0000bbbb", Name: "neu", Members: []string{"group:wwi24"}}},
		"in a new group":        {{ID: "0000aaaa", Name: "alt", Members: []string{"group:wwi24"}}, {ID: "0000bbbb", Name: "neu"}, {Name: "dritte", Members: []string{"group:wwi24"}}},
		"with a relation":       {{ID: "0000aaaa", Name: "alt", Members: []string{"group:wwi24#studierende"}}, {ID: "0000bbbb", Name: "neu"}},
		"admin is no exception": nil,
	} {
		if groups == nil {
			p, _ := s.Get(ctx, "kurs")
			p.Groups[1].Members = []string{"group:wwi25"}
			if _, err := s.Update(ctx, p, "admin", true); reason(err) == "" {
				t.Errorf("%s: accepted", name)
			}
			continue
		}
		if err := save(groups); reason(err) == "" || !strings.Contains(reason(err), "nur Personen") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Removing it is always possible.
	if err := save([]Group{{ID: "0000aaaa", Name: "alt", Members: []string{"user:a@dhbw.de"}}, {ID: "0000bbbb", Name: "neu"}}); err != nil {
		t.Fatalf("removed: %v", err)
	}
	if err := save([]Group{{ID: "0000aaaa", Name: "alt", Members: []string{"group:wwi24"}}, {ID: "0000bbbb", Name: "neu"}}); err == nil {
		t.Fatal("a removed group token came back")
	}
}

// The version of a project is its updated_at: a save with the one loaded goes through and hands out the next, a stale one is refused, none is not checked.
func TestUpdateConflict(t *testing.T) {
	ctx := context.Background()
	s := service()
	mgr := []Member{{Token: "user:m@dhbw.de", Role: RoleManager}, {Token: "user:a@dhbw.de"}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Members: mgr, StorageGB: 10, GroupChoice: true, Groups: []Group{{ID: "0000aaaa", Name: "g1"}}}, "admin")
	loaded, _ := s.Get(ctx, "kurs")
	loaded.Description = "eins"
	saved, err := s.Update(ctx, loaded, "m@dhbw.de", false)
	if err != nil || saved.Description != "eins" || !saved.UpdatedAt.After(loaded.UpdatedAt) {
		t.Fatalf("first save: %+v %v", saved, err)
	}
	if got, _ := s.Get(ctx, "kurs"); !sameVersion(got.UpdatedAt, saved.UpdatedAt) {
		t.Fatalf("the answer's version %v is not the stored one %v", saved.UpdatedAt, got.UpdatedAt)
	}
	loaded.Description = "zwei"
	if _, err := s.Update(ctx, loaded, "m@dhbw.de", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err := s.Update(ctx, loaded, "admin", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save by an admin: %v", err)
	}
	// Somebody choosing a group is a change like any other.
	saved.Description = "drei"
	if _, err := s.JoinGroup(ctx, []string{"user:a@dhbw.de"}, "a@dhbw.de", "kurs", "0000aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, saved, "m@dhbw.de", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("save over a join: %v", err)
	}
	// Without a version (older clients, admin tools): the last write wins, as before.
	saved.UpdatedAt = time.Time{}
	if out, err := s.Update(ctx, saved, "admin", true); err != nil || out.Description != "drei" {
		t.Fatalf("without version: %+v %v", out, err)
	}
	if got, _ := s.Get(ctx, "kurs"); got.UpdatedBy != "admin" {
		t.Fatalf("not saved: %+v", got)
	}
}

// racyStore lets another write happen between Update's first read and its write, as a join during the slower checks of a save would.
type racyStore struct {
	*MemoryStore
	between func()
}

func (r *racyStore) Modify(ctx context.Context, slug string, fn func(p *Project) error) (Project, error) {
	if r.between != nil {
		r.between()
		r.between = nil
	}
	return r.MemoryStore.Modify(ctx, slug, fn)
}

// The version is checked again where the project is written: a change between the first check and the write is not overwritten either.
func TestUpdateConflictAtWrite(t *testing.T) {
	ctx := context.Background()
	st := &racyStore{MemoryStore: NewMemoryStore()}
	s := NewService(st, Config{}, nil)
	_, _ = s.create(ctx, Project{Slug: "kurs", Members: []Member{{Token: "user:m@dhbw.de", Role: RoleManager}}}, "admin")
	loaded, _ := s.Get(ctx, "kurs")
	st.between = func() {
		p, _ := st.Get(ctx, "kurs")
		p.Description = "dazwischen"
		_ = st.Update(ctx, &p)
	}
	loaded.Description = "meins"
	if _, err := s.Update(ctx, loaded, "m@dhbw.de", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("change in between overwritten: %v", err)
	}
	if got, _ := s.Get(ctx, "kurs"); got.Description != "dazwischen" {
		t.Fatalf("stored: %q", got.Description)
	}
}

// The group choice: members and readers join one open group with a free place and leave it again, only with their own token; managers, outsiders, a closed choice, a full or archived group and a second group are refused.
func TestGroupChoice(t *testing.T) {
	ctx := context.Background()
	s := service()
	members := []Member{{Token: "user:m@dhbw.de", Role: RoleManager}, {Token: "user:a@dhbw.de"}, {Token: "user:b@dhbw.de", Role: RoleReader}, {Token: "group:wwi24"}}
	_, err := s.create(ctx, Project{Slug: "kurs", Name: "Kurs", Members: members, StorageGB: 10, GroupMaxMembers: 2, Groups: []Group{
		{ID: "0000aaaa", Name: "g1", Members: []string{"user:x@dhbw.de"}},
		{ID: "0000bbbb", Name: "g2"},
		{ID: "0000cccc", Name: "alt", Archived: true},
		{ID: "0000dddd", Name: "kursgruppe", Members: []string{"group:wwi24"}},
	}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := []string{"user:a@dhbw.de"}, []string{"user:b@dhbw.de"}, []string{"user:c@dhbw.de", "group:wwi24"}
	join := func(tokens []string, person, group string) error {
		_, err := s.JoinGroup(ctx, tokens, person, "kurs", group)
		return err
	}
	// Closed: nobody chooses.
	if err := join(a, "a@dhbw.de", "0000aaaa"); !strings.Contains(reason(err), "nicht offen") {
		t.Fatalf("closed: %v", err)
	}
	p, _ := s.Get(ctx, "kurs")
	p.GroupChoice = true
	if _, err := s.Update(ctx, p, "m@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "kurs"); !got.GroupChoice || got.GroupMaxMembers != 2 {
		t.Fatalf("a manager's switch not kept: %+v", got)
	}
	for name, c := range map[string]struct {
		tokens        []string
		person, group string
		want          string
	}{
		"outsider":  {[]string{"user:z@dhbw.de"}, "z@dhbw.de", "0000aaaa", "not found"},
		"manager":   {[]string{"user:m@dhbw.de"}, "m@dhbw.de", "0000aaaa", "Verwaltung"},
		"archived":  {a, "a@dhbw.de", "0000cccc", "gibt es"},
		"unknown":   {a, "a@dhbw.de", "0000eeee", "gibt es"},
		"no e-mail": {a, "", "0000aaaa", "E-Mail"},
	} {
		err := join(c.tokens, c.person, c.group)
		if got := reason(err); !strings.Contains(got, c.want) && !(c.want == "not found" && errors.Is(err, ErrNotFound)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a joins g1 (with x: full now), with an e-mail address in capitals; the reader b finds it full and takes g2.
	seats, err := s.JoinGroup(ctx, a, "A@DHBW.de", "kurs", "0000aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 3 || seats[0].Count != 2 || seats[0].Max != 2 || !seats[0].Mine || !slices.Equal(seats[0].Members, []string{"user:x@dhbw.de", "user:a@dhbw.de"}) {
		t.Fatalf("seats after joining: %+v", seats)
	}
	if seats[1].Mine || seats[1].Members != nil || seats[2].Count != 0 || seats[2].Name != "kursgruppe" {
		t.Fatalf("other groups show too much: %+v", seats)
	}
	if err := join(b, "b@dhbw.de", "0000aaaa"); !strings.Contains(reason(err), "voll") {
		t.Fatalf("full: %v", err)
	}
	if err := join(b, "b@dhbw.de", "0000bbbb"); err != nil {
		t.Fatalf("reader joins: %v", err)
	}
	// Once in a group, another one only after leaving; the own group again changes nothing.
	if err := join(a, "a@dhbw.de", "0000bbbb"); !strings.Contains(reason(err), "Schon in Gruppe „g1“") {
		t.Fatalf("second group: %v", err)
	}
	before, _ := s.Get(ctx, "kurs")
	if err := join(a, "a@dhbw.de", "0000aaaa"); err != nil {
		t.Fatalf("own group again: %v", err)
	}
	if after, _ := s.Get(ctx, "kurs"); !sameVersion(after.UpdatedAt, before.UpdatedAt) {
		t.Fatal("joining one's own group again wrote the project")
	}
	// In a group through the course group: counted as in it, not to be left here.
	if err := join(c, "c@dhbw.de", "0000bbbb"); !strings.Contains(reason(err), "kursgruppe") {
		t.Fatalf("in a group through the course: %v", err)
	}
	if _, err := s.LeaveGroup(ctx, c, "c@dhbw.de", "kurs", "0000dddd"); !strings.Contains(reason(err), "Kursgruppe") {
		t.Fatalf("leaving the course's group: %v", err)
	}
	// Leaving takes out only the person's own token; then the other group is open to them.
	seats, err = s.LeaveGroup(ctx, a, "a@dhbw.de", "kurs", "0000aaaa")
	if err != nil || seats[0].Mine || seats[0].Count != 1 || seats[0].Members != nil {
		t.Fatalf("leave: %+v %v", seats, err)
	}
	if got, _ := s.Get(ctx, "kurs"); !slices.Equal(got.Groups[0].Members, []string{"user:x@dhbw.de"}) || got.UpdatedBy != "a@dhbw.de" {
		t.Fatalf("after leaving: %+v", got.Groups[0])
	}
	if _, err := s.LeaveGroup(ctx, a, "a@dhbw.de", "kurs", "0000aaaa"); err != nil {
		t.Fatalf("leaving again: %v", err)
	}
	if err := join(a, "a@dhbw.de", "0000bbbb"); err != nil {
		t.Fatalf("join after leaving: %v", err)
	}
	// The members see the seats in their projects; the managers too, besides every group.
	mine, _ := s.ForPerson(ctx, a, "a@dhbw.de", false)
	if len(mine) != 1 || len(mine[0].GroupSeats) != 3 || !mine[0].GroupSeats[1].Mine || len(mine[0].Groups) != 1 || mine[0].Groups[0].Name != "g2" {
		t.Fatalf("member's view: %+v", mine)
	}
	if mine[0].GroupSeats[0].Members != nil {
		t.Fatalf("another group's members leaked: %+v", mine[0].GroupSeats[0])
	}
	// Closed again: only the managers assign, nobody sees seats.
	p, _ = s.Get(ctx, "kurs")
	p.GroupChoice = false
	if _, err := s.Update(ctx, p, "m@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeaveGroup(ctx, a, "a@dhbw.de", "kurs", "0000bbbb"); !strings.Contains(reason(err), "nicht offen") {
		t.Fatalf("leave while closed: %v", err)
	}
	if mine, _ := s.ForPerson(ctx, a, "a@dhbw.de", false); mine[0].GroupSeats != nil {
		t.Fatalf("seats while closed: %+v", mine[0].GroupSeats)
	}
}

// Many people taking the last place at once: exactly one gets it, because read, check and write are one step of the store.
func TestGroupChoiceLastPlace(t *testing.T) {
	ctx := context.Background()
	s := service()
	var members []Member
	for i := range 20 {
		members = append(members, Member{Token: fmt.Sprintf("user:p%d@dhbw.de", i)})
	}
	_, _ = s.create(ctx, Project{Slug: "kurs", Members: members, GroupChoice: true, GroupMaxMembers: 1, Groups: []Group{{ID: "0000aaaa", Name: "g1"}}}, "admin")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 20 {
		wg.Go(func() {
			if _, err := s.JoinGroup(ctx, nil, fmt.Sprintf("p%d@dhbw.de", i), "kurs", "0000aaaa"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if p, _ := s.Get(ctx, "kurs"); ok != 1 || len(p.Groups[0].Members) != 1 {
		t.Fatalf("%d joined, group %v", ok, p.Groups[0].Members)
	}
}

// A group's limit of places is checked like the other settings.
func TestGroupMaxMembers(t *testing.T) {
	for _, n := range []int{-1, MaxGroupMembers + 1} {
		if err := validateGroups(&Project{GroupMaxMembers: n}); err == nil {
			t.Errorf("%d accepted", n)
		}
	}
	if err := validateGroups(&Project{GroupMaxMembers: 4}); err != nil {
		t.Error(err)
	}
}
