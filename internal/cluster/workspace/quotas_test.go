package workspace

import (
	"slices"
	"testing"
)

func TestAutoHomeGB(t *testing.T) {
	for _, c := range []struct{ storage, homes, want int }{
		{20, 32, 2},  // 2*20/32 = 1.25 -> 2
		{100, 32, 7}, // 6.25 -> 7
		{10, 1, 0},   // one home: no limit
		{10, 0, 0},
		{10, 2, 0},  // twice the share is the volume
		{10, 3, 7},  // 6.67 -> 7
		{1, 50, 0},  // at least 1 GB = the volume
		{0, 10, 0},  // no volume
		{3, 100, 1}, // at least 1 GB
	} {
		if got := AutoHomeGB(c.storage, c.homes); got != c.want {
			t.Errorf("AutoHomeGB(%d, %d) = %d, want %d", c.storage, c.homes, got, c.want)
		}
	}
	if got := (Project{StorageGB: 20, StorageHomeGB: 5}).HomeLimitGB(32); got != 5 {
		t.Errorf("own limit: %d", got)
	}
	if got := (Project{StorageGB: 4, StorageHomeGB: 5}).HomeLimitGB(32); got != 4 {
		t.Errorf("own limit above the volume: %d", got)
	}
}

func TestManagedQuotaPath(t *testing.T) {
	for p, want := range map[string]bool{
		"ws-t-kurs/homes/a-dhbw-de---1a2b3c4d": true,
		"ws-t-kurs/groups/0000aaaa":            true,
		"ws-t-kurs/project-shared":             true,
		"ws-t-kurs":                            false, // the volume's own quota is the CSI driver's
		"home-a-dhbw-de---1a2b3c4d":            false,
		"ws-t-kurs/homes/a/b":                  false,
		"ws-t-kurs/groups/../homes":            false,
		"ws-t-kurs/other":                      false,
		"/ws-t-kurs/project-shared":            false,
	} {
		if got := ManagedQuotaPath(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestQuotaTargetsAndPlan(t *testing.T) {
	projects := []Project{
		{Slug: "t-kurs", StorageGB: 20, StorageProjectGB: 8, Members: []Member{{Token: "group:kurs-leitung", Role: RoleManager}},
			Groups: []Group{{ID: "0000aaaa", Name: "g1", StorageGB: 3}, {ID: "0000bbbb", Name: "g2"}}},
		{Slug: "t-solo", StorageGB: 10},
		{Slug: "t-ohne"},
	}
	homes := map[string][]Home{
		"t-kurs": {{Dir: "a", Tokens: []string{"user:a@dhbw.de"}}, {Dir: "b", Tokens: []string{"user:b@dhbw.de"}}, {Dir: "m", Tokens: []string{"user:m@dhbw.de", "group:kurs-leitung"}}},
		"t-solo": {{Dir: "s", Tokens: []string{"user:s@dhbw.de"}}},
	}
	got := []string{}
	for _, q := range QuotaTargets(projects, homes) {
		got = append(got, q.Path+"="+string(rune('0'+q.GiB)))
	}
	// Three homes in 20 GB: 2*20/3 = 13.3 -> 14 GB for the members, none for the manager; t-solo has one home, no limit.
	want := []string{"ws-t-kurs/groups/0000aaaa=3", "ws-t-kurs/homes/a=>", "ws-t-kurs/homes/b=>", "ws-t-kurs/project-shared=8"}
	if !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
	current := []StorageDir{
		{Dir: "ws-t-kurs", QuotaGiB: 20},                 // the volume: never touched
		{Dir: "ws-t-kurs/homes/a", QuotaGiB: 14},         // right already
		{Dir: "ws-t-kurs/homes/b", QuotaGiB: 5},          // different
		{Dir: "ws-t-kurs/homes/m", QuotaGiB: 14},         // a manager now: goes
		{Dir: "ws-t-kurs/groups/0000bbbb", QuotaGiB: 2},  // limit removed: goes
		{Dir: "home-old---1a2b3c4d", QuotaGiB: 10},       // not ours
		{Dir: "ws-t-solo/project-shared/x", QuotaGiB: 1}, // not a managed path
	}
	plan := QuotaPlan(current, QuotaTargets(projects, homes))
	wantPlan := []string{"set ws-t-kurs/groups/0000aaaa 3", "set ws-t-kurs/homes/b 14", "set ws-t-kurs/project-shared 8", "delete ws-t-kurs/groups/0000bbbb", "delete ws-t-kurs/homes/m"}
	if !slices.Equal(plan, wantPlan) {
		t.Errorf("plan %v, want %v", plan, wantPlan)
	}
}
