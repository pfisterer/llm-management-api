package workspace

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A project built from Git before the material field gets its repository as material; one without Git gets none, and a later change of the material stays. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestMigrateMaterial(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS workspace_projects")
	if _, err := NewGormStore(db); err != nil {
		t.Fatal(err)
	}
	// The table as it was before: without the material columns.
	db.Exec("ALTER TABLE workspace_projects DROP COLUMN material_git_url, DROP COLUMN material_git_ref")
	db.Exec("INSERT INTO workspace_projects (slug, name, image_git_url, image_git_ref) VALUES ('git', 'git', 'https://github.com/o/kurs', 'v3'), ('frei', 'frei', '', '')")
	s, err := NewGormStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := s.Get(ctx, "git")
	if err != nil || p.MaterialGitURL != "https://github.com/o/kurs" || p.MaterialGitRef != "v3" {
		t.Fatalf("git: %+v %v", p, err)
	}
	if p, err := s.Get(ctx, "frei"); err != nil || p.MaterialGitURL != "" {
		t.Fatalf("frei: %+v %v", p, err)
	}
	p.MaterialGitURL = ""
	if err := s.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGormStore(db); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Get(ctx, "git"); p.MaterialGitURL != "" {
		t.Fatalf("migrated again: %+v", p)
	}
}

// The release times are new columns: rows from before (NULL there) read as nothing set, and set times come back as stored, an explicit 0 (no upper bound) included. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestReleaseColumns(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS workspace_projects")
	s, err := NewGormStore(db)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("ALTER TABLE workspace_projects DROP COLUMN release_times, DROP COLUMN release_max, DROP COLUMN tree_release")
	db.Exec("INSERT INTO workspace_projects (slug, name) VALUES ('alt', 'alt')")
	if s, err = NewGormStore(db); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := s.Get(ctx, "alt")
	if err != nil || p.Release.GPU.MaxHours != nil || p.ReleaseMax.CPU.IdleOpenMinutes != nil || p.TreeRelease {
		t.Fatalf("row from before: %+v %v", p, err)
	}
	zero, ten := 0, 10
	p.Release.GPU = ReleaseTimes{MaxHours: &zero, IdleClosedMinutes: &ten}
	p.ReleaseMax.GPU.MaxHours = &zero
	if err := s.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.Get(ctx, "alt"); p.Release.GPU.MaxHours == nil || *p.Release.GPU.MaxHours != 0 || *p.Release.GPU.IdleClosedMinutes != 10 || p.Release.GPU.IdleOpenMinutes != nil || *p.ReleaseMax.GPU.MaxHours != 0 {
		t.Fatalf("round trip: %+v", p.Release)
	}
}
