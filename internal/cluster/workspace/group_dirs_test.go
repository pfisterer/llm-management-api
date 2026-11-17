package workspace

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// Only exact group folders below a project's volume reach the plan, whatever the job sends.
func TestParseGroupDirs(t *testing.T) {
	got := ParseGroupDirs("ws-kurs/groups/0000aaaa 1048576\n/ws-kurs/groups/0000bbbb/\nws-kurs/groups/0000cccc x\n" +
		"ws-kurs 5\nws-kurs/project-shared 1\nws-kurs/homes/a-b 1\nws-kurs/groups 1\nws-kurs/groups/0000AAAA 1\nws-kurs/groups/../homes 1\n" +
		"ws-kurs/groups/0000aaaa/sub 1\nws-kurs/groups/0000aaa 1\nhome-x/groups/0000aaaa 1\nws-Kurs/groups/0000aaaa 1\nws-/groups/0000aaaa 1\n\n")
	want := []GroupDir{{Path: "ws-kurs/groups/0000aaaa", UsedGiB: 1}, {Path: "ws-kurs/groups/0000bbbb"}, {Path: "ws-kurs/groups/0000cccc"}}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}

// The plan removes the folders of groups a project no longer has, and nothing else: archived groups keep theirs, an unknown or ended project or one without storage is left alone; switched off it only says what would go.
func TestGroupDirPlan(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ended := now.AddDate(0, 0, -3)
	projects := []Project{
		{Slug: "kurs", StorageGB: 10, Groups: []Group{{ID: "0000aaaa", Name: "g1"}, {ID: "0000bbbb", Name: "alt", Archived: true}}},
		{Slug: "vorbei", StorageGB: 10, EndsAt: &ended},
		{Slug: "ohne", StorageGB: 0},
	}
	dirs := []GroupDir{
		{Path: "ws-kurs/groups/0000aaaa"}, // a group of the project
		{Path: "ws-kurs/groups/0000bbbb"}, // archived: kept
		{Path: "ws-kurs/groups/0000cccc"}, // removed: goes
		{Path: "ws-kurs/groups/0000cccc"}, // twice: once
		{Path: "ws-vorbei/groups/0000dddd"},
		{Path: "ws-ohne/groups/0000dddd"},
		{Path: "ws-fremd/groups/0000dddd"},
		{Path: "ws-kurs/homes/x"},
		{Path: "ws-kurs/project-shared"},
		{Path: "ws-kurs"},
		{Path: "ws-kurs/groups/0000eeee/../../homes"},
	}
	if got := GroupDirPlan(projects, dirs, true, now); !slices.Equal(got, []string{"remove ws-kurs/groups/0000cccc"}) {
		t.Errorf("plan: %v", got)
	}
	if got := GroupDirPlan(projects, dirs, false, now); !slices.Equal(got, []string{"would-remove ws-kurs/groups/0000cccc"}) {
		t.Errorf("switched off: %v", got)
	}
	if got := GroupDirPlan(nil, dirs, true, now); len(got) != 0 {
		t.Errorf("no projects known: %v", got)
	}
}

// failingStore cannot read its projects.
type failingStore struct{ *MemoryStore }

func (failingStore) List(context.Context) ([]Project, error) { return nil, errors.New("database down") }

// Through the service: the sizes are kept for the managers (and come back after a restart of the same store), and when the projects cannot be read there is no plan at all.
func TestGroupDirPlanFor(t *testing.T) {
	ctx := context.Background()
	s := service()
	mgr := []Member{{Token: "user:m@dhbw.de", Role: RoleManager}, {Token: "user:a@dhbw.de"}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Members: mgr, StorageGB: 10, Groups: []Group{{ID: "0000aaaa", Name: "g1", Members: []string{"user:a@dhbw.de"}}}}, "admin")
	plan, err := s.GroupDirPlanFor(ctx, ParseGroupDirs("ws-kurs/groups/0000aaaa 2097152\nws-kurs/groups/0000ffff 10\n"), true)
	if err != nil || !slices.Equal(plan, []string{"remove ws-kurs/groups/0000ffff"}) {
		t.Fatalf("plan %v %v", plan, err)
	}
	mine, _ := s.ForPerson(ctx, []string{"user:m@dhbw.de"}, "m@dhbw.de", false)
	if g := mine[0].Groups[0]; g.UsedGiB == nil || *g.UsedGiB != 2 {
		t.Fatalf("manager sees no size: %+v", g)
	}
	member, _ := s.ForPerson(ctx, []string{"user:a@dhbw.de"}, "a@dhbw.de", false)
	if g := member[0].Groups[0]; g.UsedGiB != nil {
		t.Fatalf("member sees the size: %+v", g)
	}
	// A size a client sends is never stored.
	p, _ := s.Get(ctx, "kurs")
	x := 99.0
	p.Groups[0].UsedGiB = &x
	if out, err := s.Update(ctx, p, "admin", true); err != nil || out.Groups[0].UsedGiB != nil {
		t.Fatalf("stored a size: %+v %v", out.Groups, err)
	}
	broken := NewService(failingStore{NewMemoryStore()}, Config{}, nil)
	if plan, err := broken.GroupDirPlanFor(ctx, []GroupDir{{Path: "ws-kurs/groups/0000ffff"}}, true); err == nil || plan != nil {
		t.Fatalf("plan without projects: %v %v", plan, err)
	}
}
