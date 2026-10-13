package workspace

import (
	"context"
	"math"
	"testing"
	"time"
)

// The hourly report: directories with used space, quota and inodes as juicefs 1.4 prints them, and the Valkey memory.
func TestStorageReport(t *testing.T) {
	ctx := context.Background()
	s := service()
	st, err := s.UpsertFromTree(ctx, "p_abc", TreeProject{Name: "Kurs", StorageGB: 50})
	if err != nil {
		t.Fatal(err)
	}
	table := `+-------------------------------------------+--------+---------+------+-----------+-------+-------+
|                  Path/ID                  |  Size  |   Used  | Use% |   Inodes  | IUsed | IUse% |
+-------------------------------------------+--------+---------+------+-----------+-------+-------+
| /home-dennis-pfisterer-dhbw-de---c771a8ee | 10 GiB | 512 MiB |   5% | unchanged |   231 |       |
| /ws-t-abc                                 | 50 GiB | 2.5 GiB |   5% | unchanged |  9000 |       |
+-------------------------------------------+--------+---------+------+-----------+-------+-------+`
	dirs, ok := ParseQuotaTable(table)
	if !ok || len(dirs) != 2 {
		t.Fatalf("parsed %+v", dirs)
	}
	_ = s.SetStorageDirs(ctx, dirs)
	if m, ok := ParseValkeyInfo("# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\nused_memory_peak:2097152\r\nmaxmemory:0\r\ntotal_system_memory:16000000000\r\n"); !ok || m.UsedBytes != 1<<20 || m.PeakBytes != 2<<20 || m.SystemBytes != 16e9 {
		t.Fatalf("valkey %+v", m)
	} else {
		_ = s.SetValkeyMemory(ctx, m)
	}
	r, err := s.StorageReport(ctx)
	if err != nil || len(r.Dirs) != 2 || r.Dirs[0].Project != st.Slug || r.Dirs[0].Name != "Kurs" || r.Dirs[0].QuotaGiB != 50 || r.Dirs[0].Inodes != 9000 || math.Abs(r.UsedGiB-3) > 1e-9 || r.Valkey == nil || r.DirsAt == nil {
		t.Fatalf("report %+v %v", r, err)
	}
	// The tree sees what the project's volume holds.
	got, _ := s.TreeProjects(ctx)
	if len(got) != 1 || len(got[0].Used) != 1 || got[0].Used[0].Unit != "storage_gb" || got[0].Used[0].Amount != 50 || got[0].Used[0].Used != 2.5 {
		t.Fatalf("tree status %+v", got)
	}
	if _, ok := ParseQuotaTable("nothing"); ok {
		t.Fatal("a text without a table must not count as a report")
	}
}

// A volume is named for deletion only after its project has been missing for two passes; a project that is back keeps it.
func TestOrphans(t *testing.T) {
	ctx := context.Background()
	s := service()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	st, _ := s.UpsertFromTree(ctx, "p_keep", TreeProject{Name: "Bleibt", StorageGB: 10})
	vols := []StorageVolume{{PVC: "ws-" + st.Slug, Project: st.Slug}, {PVC: "ws-t-gone", Project: "t-gone"}, {PVC: "ws-x", Project: ""}}
	if o, _ := s.Orphans(ctx, vols); len(o) != 0 {
		t.Fatalf("first pass deletes %+v", o)
	}
	now = now.Add(5 * time.Minute)
	if o, _ := s.Orphans(ctx, vols); len(o) != 0 {
		t.Fatalf("too early: %+v", o)
	}
	now = now.Add(5 * time.Minute)
	if o, _ := s.Orphans(ctx, vols); len(o) != 1 || o[0].PVC != "ws-t-gone" {
		t.Fatalf("after two passes: %+v", o)
	}
	// Deleting the kept project names its volume only after the grace as well.
	if err := s.DeleteFromTree(ctx, "p_keep"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Orphans(ctx, vols[:1]); len(o) != 0 {
		t.Fatalf("right after deleting: %+v", o)
	}
	now = now.Add(StorageGrace)
	if o, _ := s.Orphans(ctx, vols[:1]); len(o) != 1 {
		t.Fatalf("after the grace: %+v", o)
	}
}
