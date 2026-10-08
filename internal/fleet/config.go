package fleet

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
)

// Profile is a machine class by memory: the first profile a machine's RAM
// reaches decides its model.
type Profile struct {
	Name         string  `json:"name"`
	MinRAMMb     int     `json:"minRamMb"`
	Model        string  `json:"model"`
	WiredLimitMb int     `json:"wiredLimitMb"`
	Context      int     `json:"context"`
	Weight       float64 `json:"weight"`
}

// Override changes the assignment for one serial number.
type Override struct {
	Model        *string  `json:"model"`
	Context      *int     `json:"context"`
	WiredLimitMb *int     `json:"wiredLimitMb"`
	Weight       *float64 `json:"weight"`
	Gate         *string  `json:"gate"`
	Enabled      *bool    `json:"enabled"`
}

// Config is the `fleet` block of the llm-aas inventory, unchanged (env FLEET).
type Config struct {
	Enabled          bool                `json:"enabled"`
	Pool             string              `json:"pool"`
	EnrollHostname   string              `json:"enrollHostname"`
	EnrollToken      string              `json:"enrollToken"`
	AdminToken       string              `json:"adminToken"`
	AllowedSerials   []string            `json:"allowedSerials"`
	SelfUpdate       bool                `json:"selfUpdate"`
	SelfUpdateCanary []string            `json:"selfUpdateCanary"`
	OllamaVersion    string              `json:"ollamaVersion"`
	ReenrollSeconds  int                 `json:"reenrollSeconds"`
	Port             int                 `json:"port"`
	APIKey           string              `json:"apiKey"`
	LiteLLMParams    map[string]any      `json:"litellmParams"`
	Overrides        map[string]Override `json:"overrides"`
	Profiles         []Profile           `json:"profiles"`
}

// WireGuard is the `wireguard` block of the inventory (env WIREGUARD).
type WireGuard struct {
	HubPublicKey   string `json:"hubPublicKey"`
	ListenPort     int    `json:"listenPort"`
	Network        string `json:"network"`
	HubAddress     string `json:"hubAddress"`
	EndpointHostV6 string `json:"endpointHostV6"`
	EndpointHostV4 string `json:"endpointHostV4"`
}

// ParseConfig reads both JSON blocks and checks the address pool against the
// WireGuard network — the same start-up checks the broker made.
func ParseConfig(fleetJSON, wgJSON string) (Config, WireGuard, error) {
	var c Config
	var w WireGuard
	if fleetJSON != "" {
		if err := json.Unmarshal([]byte(fleetJSON), &c); err != nil {
			return c, w, fmt.Errorf("FLEET: %w", err)
		}
	}
	if wgJSON != "" {
		if err := json.Unmarshal([]byte(wgJSON), &w); err != nil {
			return c, w, fmt.Errorf("WIREGUARD: %w", err)
		}
	}
	if c.Pool == "" {
		c.Pool = "10.90.4.0/22"
	}
	if c.Port == 0 {
		c.Port = 11434
	}
	if c.ReenrollSeconds == 0 {
		c.ReenrollSeconds = 3600
	}
	if w.ListenPort == 0 {
		w.ListenPort = 51820
	}
	if w.HubAddress == "" {
		w.HubAddress = "10.90.0.1"
	}
	if w.Network == "" {
		w.Network = "10.90.0.0/16"
	}
	sort.SliceStable(c.Profiles, func(i, j int) bool { return c.Profiles[i].MinRAMMb > c.Profiles[j].MinRAMMb })
	if !c.Enabled {
		return c, w, nil
	}
	pool, err := netip.ParsePrefix(c.Pool)
	if err != nil || pool.Masked() != pool || !pool.Addr().Is4() {
		return c, w, fmt.Errorf("fleet pool %q is not an IPv4 network without host bits", c.Pool)
	}
	outer, err := netip.ParsePrefix(w.Network)
	if err != nil {
		return c, w, fmt.Errorf("wireguard network %q: %w", w.Network, err)
	}
	if !outer.Contains(pool.Addr()) || pool.Bits() < outer.Bits() {
		return c, w, fmt.Errorf("fleet pool %s lies outside the WireGuard network %s", pool, outer)
	}
	hub, err := netip.ParseAddr(w.HubAddress)
	if err != nil {
		return c, w, fmt.Errorf("hub address %q: %w", w.HubAddress, err)
	}
	hub24 := netip.PrefixFrom(hub, 24).Masked()
	if pool.Overlaps(hub24) {
		return c, w, fmt.Errorf("fleet pool %s overlaps the curated /24 around the hub (%s)", pool, hub24)
	}
	return c, w, nil
}

func (c Config) profileFor(ramMb int) (Profile, bool) {
	for _, p := range c.Profiles { // sorted high to low
		if ramMb >= p.MinRAMMb {
			return p, true
		}
	}
	return Profile{}, false
}

// Assignment is what a machine is told to run.
type Assignment struct {
	Profile      string
	Model        string
	Context      int
	WiredLimitMb int
	Weight       float64
	Gate         string
	Enabled      bool
}

// assignment applies the per-serial override on top of the profile.
func (c Config) assignment(p Profile, serial string) Assignment {
	a := Assignment{Profile: p.Name, Model: p.Model, Context: p.Context, WiredLimitMb: p.WiredLimitMb,
		Weight: p.Weight, Gate: "idle-only", Enabled: true}
	if a.Weight == 0 {
		a.Weight = 1
	}
	if o, ok := c.Overrides[serial]; ok {
		if o.Model != nil {
			a.Model = *o.Model
		}
		if o.Context != nil {
			a.Context = *o.Context
		}
		if o.WiredLimitMb != nil {
			a.WiredLimitMb = *o.WiredLimitMb
		}
		if o.Weight != nil {
			a.Weight = *o.Weight
		}
		if o.Gate != nil {
			a.Gate = *o.Gate
		}
		if o.Enabled != nil {
			a.Enabled = *o.Enabled
		}
	}
	return a
}
