// Package usage books what workspaces (Jupyter servers, GPU kernels) reserve, per day, project, person, profile and GPU class, and reports it like openstack-management-api's usage report: separate units (vCPU-h, RAM-GB-h, GPU-h per class, home GB-days), no combined unit. The source is Kubernetes, sampled every minute; billed is the reservation (requests), the measured use from metrics-server is kept next to it.
package usage

import (
	"context"
	"time"
)

// Row is one UTC day of one person in one project, profile and GPU class. No foreign keys: rows outlive projects and access rules, like project_usage_daily in openstack-management-api.
type Row struct {
	Day      time.Time `json:"day" gorm:"primaryKey;type:date"`
	Project  string    `json:"project" gorm:"primaryKey"` // until projects exist (W2): the access rule that grants compute, e.g. group:wwi23seb
	Person   string    `json:"person" gorm:"primaryKey"`
	Profile  string    `json:"profile" gorm:"primaryKey"`
	GPUClass string    `json:"gpu_class" gorm:"primaryKey"` // empty without GPU

	ServerHours    float64 `json:"server_hours"`      // pod running
	VCPUHours      float64 `json:"vcpu_hours"`        // requested cores × hours
	RAMGBHours     float64 `json:"ram_gb_hours"`      // requested GiB × hours
	GPUHours       float64 `json:"gpu_hours"`         // GPUs × hours, in the row's GPU class
	CPUUsedHours   float64 `json:"cpu_used_hours"`    // measured cores × hours (metrics-server); 0 when unavailable
	RAMUsedGBHours float64 `json:"ram_used_gb_hours"` // measured GiB × hours
	HomeGB         float64 `json:"home_gb"`           // home size, largest sample of the day (on the profile "home" row)

	UpdatedAt time.Time `json:"-"`
}

func (Row) TableName() string { return "workspace_usage_daily" }

// Store keeps the rows. Add adds the numbers to an existing row (or creates it); HomeGB keeps the larger value.
type Store interface {
	Add(ctx context.Context, rows []Row) error
	Range(ctx context.Context, from, to time.Time) ([]Row, error)
	// Persons lists everyone who has a row, for matching home directories to people.
	Persons(ctx context.Context) ([]string, error)
}

// Day is the UTC date of t, as stored.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
