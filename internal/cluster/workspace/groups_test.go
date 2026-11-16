package workspace

import (
	"slices"
	"testing"
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
		"reader in g1":         {[]string{"user:a@dhbw.de"}, RoleReader, []string{"dhbw/project=project-shared:ro", "dhbw/groups/g1=groups/0000aaaa:ro"}},
		"no group":             {[]string{"user:c@dhbw.de"}, RoleMember, []string{"dhbw/project=project-shared:rw"}},
		"manager sees all":     {[]string{"user:m@dhbw.de"}, RoleManager, []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw", "dhbw/groups/g2=groups/0000bbbb:rw", "dhbw/groups/alt=groups/0000cccc:ro"}},
		"admin like a manager": {nil, "admin", []string{"dhbw/project=project-shared:rw", "dhbw/groups/g1=groups/0000aaaa:rw", "dhbw/groups/g2=groups/0000bbbb:rw", "dhbw/groups/alt=groups/0000cccc:ro"}},
		"not a member":         {[]string{"user:a@dhbw.de"}, "", []string{}},
	} {
		if got := mounts(p.AreasFor(c.tokens, c.role)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if got := (Project{Groups: p.Groups}).AreasFor([]string{"user:a@dhbw.de"}, RoleMember); got != nil {
		t.Errorf("without storage: %v", got)
	}
}
