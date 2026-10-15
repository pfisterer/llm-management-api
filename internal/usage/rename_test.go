package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A class renamed while samples still came in under the old id: both ids hold the same hour and day, and the rename adds them up instead of failing on the key. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestRenameGPUClassMerges(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"workspace_usage_daily", "gpu_usage_hourly", "gpu_usage_hourly_person"} {
		db.Exec("DROP TABLE IF EXISTS " + tbl)
	}
	s, err := NewGormStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hour := Hour(time.Date(2026, 10, 14, 19, 5, 0, 0, time.UTC))
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, class := range []string{"allgemein-klein", "general-small"} {
		if err := s.AddGPUHours(ctx, []GPUHour{{Hour: hour, GPUClass: class, SampledHours: 0.5, Capacity: 1, MaxWaiting: 1}}, []GPUHourPerson{{Hour: hour, GPUClass: class, Project: "p", Person: "a@x", Profile: "gpu", GPUHours: 0.5}}); err != nil {
			t.Fatal(err)
		}
		if err := s.Add(ctx, []Row{{Day: day, Project: "p", Person: "a@x", Profile: "gpu", GPUClass: class, GPUHours: 2, HomeGB: 3}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenameGPUClass(ctx, "allgemein-klein", "general-small"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameGPUClass(ctx, "allgemein-klein", "general-small"); err != nil {
		t.Fatal("second rename:", err)
	}
	hours, people, err := s.GPUHours(ctx, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 1 || hours[0].GPUClass != "general-small" || hours[0].Capacity != 2 || hours[0].MaxWaiting != 1 {
		t.Fatalf("hours = %+v", hours)
	}
	if len(people) != 1 || people[0].GPUHours != 1 {
		t.Fatalf("people = %+v", people)
	}
	rows, err := s.Range(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GPUClass != "general-small" || rows[0].GPUHours != 4 || rows[0].HomeGB != 3 {
		t.Fatalf("rows = %+v", rows)
	}
}
