// Package fleet is the registry of self-enrolled inference machines (the Mac
// fleet, plus Linux hosts using the same agent) and everything around it:
// enrolment, the WireGuard peer list for the hub, the site list for the
// discovery job, script self-update and the JAMF material.
//
// Ported from the Node key broker. The registry moved from a JSON file on the
// broker's volume to Postgres; the rules stayed the same.
package fleet

import (
	"strings"
	"time"
)

// Peer is one enrolled machine. Field names follow the broker's JSON registry
// so an import is a 1:1 copy (see Import).
type Peer struct {
	Serial       string `json:"serial" gorm:"primaryKey"`
	Name         string `json:"name" gorm:"index"`
	Address      string `json:"address"`
	PublicKey    string `json:"publicKey"`
	PresharedKey string `json:"presharedKey"`
	Transport    string `json:"transport"`
	APIBase      string `json:"apiBase,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`

	Hostname  string `json:"hostname"`
	PrimaryIP string `json:"primaryIp"` // address of the default route's interface, as the machine reports it
	Hardware  string `json:"hardware"`
	OSVersion string `json:"osVersion"`
	OS        string `json:"os"`
	GPU       string `json:"gpu"`
	Location  string `json:"location"`
	Operator  string `json:"operator"`
	Contact   string `json:"contact"`
	RAMMb     int    `json:"ramMb"`
	Port      int    `json:"port,omitempty"`

	// What the machine reports to serve, space-separated (models never contain spaces).
	ModelsList string `json:"-" gorm:"column:models"`
	Controls   bool   `json:"controls"`
	Busy       bool   `json:"busy"`
	Scripts    string `json:"scripts"`
	PkgVersion string `json:"pkgVersion"`

	// The effective assignment (profile + overrides) at the last enrolment.
	Profile string  `json:"profile"`
	Model   string  `json:"model"`
	Weight  float64 `json:"weight"`
	Gate    string  `json:"gate"`
	Enabled bool    `json:"enabled"`

	Blocked    bool       `json:"blocked"`
	EnrolledAt time.Time  `json:"enrolledAt"`
	LastSeen   *time.Time `json:"lastSeen"`
}

// TableName pins the table name.
func (Peer) TableName() string { return "fleet_peers" }

// Models returns the reported models.
func (p Peer) Models() []string {
	if strings.TrimSpace(p.ModelsList) == "" {
		return []string{}
	}
	return strings.Fields(p.ModelsList)
}

// SetModels stores the reported models.
func (p *Peer) SetModels(m []string) { p.ModelsList = strings.Join(m, " ") }
