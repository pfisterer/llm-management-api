package usage

import (
	"testing"

	"gorm.io/gorm/schema"
	"sync"
)

func TestColumnNames(t *testing.T) {
	s, err := schema.Parse(&Row{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"day", "project", "person", "profile", "gpu_class", "server_hours", "vcpu_hours", "ram_gb_hours", "gpu_hours", "cpu_used_hours", "ram_used_gb_hours", "home_gb"} {
		if s.LookUpField(c) == nil {
			t.Errorf("no column %s (the upsert in GormStore.Add names it)", c)
		}
	}
}
