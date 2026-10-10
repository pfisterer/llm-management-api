package fleet

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- hub

// HubPeer is one entry for the WireGuard hub sync (wg-fleet-sync).
type HubPeer struct {
	Name         string `json:"name"`
	Serial       string `json:"serial"`
	Address      string `json:"address"`
	PublicKey    string `json:"publicKey"`
	PresharedKey string `json:"presharedKey"`
}

// HubPeers lists tunnelled, unblocked machines. Direct machines have no key:
// a peer without PublicKey would make `wg syncconf` reject the whole file and
// drop every other peer's connection.
func (s *Service) HubPeers(ctx context.Context) (map[string]any, error) {
	peers, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []HubPeer{}
	for _, p := range peers {
		if p.Blocked || p.Transport == "direct" {
			continue
		}
		out = append(out, HubPeer{p.Name, p.Serial, p.Address, p.PublicKey, p.PresharedKey})
	}
	return map[string]any{"pool": s.cfg.Pool, "hubAddress": s.wg.HubAddress, "peers": out}, nil
}

// ---------------------------------------------------------------- discovery

// Site is one machine in the shape the discovery job expects for gpuSites.
type Site struct {
	Name          string         `json:"name"`
	Weight        float64        `json:"weight"`
	LiteLLMParams map[string]any `json:"litellmParams"`
	Fleet         bool           `json:"fleet"`
	Transport     string         `json:"transport"`
	WGAddress     string         `json:"wgAddress,omitempty"`
	Port          int            `json:"port,omitempty"`
	APIBase       string         `json:"apiBase,omitempty"`
	APIKey        string         `json:"apiKey"`
}

// Sites lists machines that should receive work: not blocked, not switched
// off by override, and not busy (the gate stopped the service because someone
// works at the machine — it must not be woken by health checks either).
func (s *Service) Sites(ctx context.Context) ([]Site, error) {
	peers, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	params := s.cfg.LiteLLMParams
	if params == nil {
		params = map[string]any{}
	}
	out := []Site{}
	for _, p := range peers {
		if p.Blocked || !p.Enabled || p.Busy {
			continue
		}
		weight := p.Weight
		if weight == 0 {
			weight = 1
		}
		site := Site{Name: p.Name, Weight: weight, LiteLLMParams: params, Fleet: true}
		if p.Transport == "direct" {
			site.Transport, site.APIBase, site.APIKey = "direct", p.APIBase, p.APIKey
		} else {
			// The port comes from the machine when it names one (vLLM: 8000).
			port := p.Port
			if port == 0 {
				port = s.cfg.Port
			}
			site.Transport, site.WGAddress, site.Port, site.APIKey = "wireguard", p.Address, port, s.cfg.APIKey
		}
		out = append(out, site)
	}
	for _, src := range s.extra {
		more, err := src(ctx)
		if err != nil {
			return nil, err
		}
		for _, site := range more {
			if site.LiteLLMParams == nil {
				site.LiteLLMParams = params
			}
			site.Fleet = true
			out = append(out, site)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- admin view

// Health is LiteLLM's view of one machine (by tunnel address).
type Health struct {
	OK, Bad int
	Models  []string
	Error   string
}

// HealthSource returns health per tunnel address; nil when unavailable.
type HealthSource func(ctx context.Context) map[string]Health

// PeerView is one row of the fleet table.
type PeerView struct {
	Serial         string     `json:"serial"`
	Name           string     `json:"name"`
	Address        string     `json:"address"`
	PrimaryIP      string     `json:"primary_ip"` // the machine's own address in its local network (default route), as it reports it at enrolment; empty until a script that sends it has enrolled
	Transport      string     `json:"transport"`
	APIBase        string     `json:"api_base,omitempty"`
	OS             string     `json:"os"`
	Hostname       string     `json:"hostname"`
	Hardware       string     `json:"hardware"`
	RAMGb          int        `json:"ram_gb"`
	Location       string     `json:"location"`
	Operator       string     `json:"operator"`
	Contact        string     `json:"contact"`
	Profile        string     `json:"profile"`
	Model          string     `json:"model"`
	Models         []string   `json:"models"`
	ModelState     string     `json:"model_state" enums:"fremdverwaltet,erfüllt,abweichend,unbekannt"`
	Serving        []string   `json:"serving"`
	Inference      string     `json:"inference" enums:"angehalten,unbekannt,nicht registriert,erreichbar,gestört,teilweise"`
	InferenceError string     `json:"inference_error,omitempty"`
	Gate           string     `json:"gate"`
	Enabled        bool       `json:"enabled"`
	Busy           bool       `json:"busy"`
	Controls       bool       `json:"controls"`
	ScriptState    string     `json:"script_state"`
	PkgVersion     string     `json:"pkg_version"`
	Canary         bool       `json:"canary"`
	LastSeen       *time.Time `json:"last_seen"`
	Blocked        bool       `json:"blocked"`
	State          string     `json:"state" enums:"aktiv,ohne Rückmeldung,gesperrt"`
	QuietSeconds   int        `json:"quiet_seconds"`
}

// FleetView is the admin page's data.
type FleetView struct {
	Enabled         bool         `json:"enabled"`
	Pool            string       `json:"pool"`
	Capacity        int          `json:"capacity"`
	EnrollHost      string       `json:"enroll_host"`
	EnrollToken     string       `json:"enroll_token"`
	ReenrollSeconds int          `json:"reenroll_seconds"`
	EndpointV6      string       `json:"endpoint_v6"`
	Endpoint        string       `json:"endpoint"` // the name machines dial (IPv4 name when set), without port
	ListenPort      int          `json:"listen_port"`
	AllowedSerials  int          `json:"allowed_serials"`
	Package         *PackageInfo `json:"package"`
	PackageID       string       `json:"package_id"`
	SelfUpdate      struct {
		Mode    string            `json:"mode"`
		Canary  []string          `json:"canary"`
		Scripts map[string]string `json:"scripts"`
	} `json:"self_update"`
	Profiles []Profile  `json:"profiles"`
	Peers    []PeerView `json:"peers"`
}

func addrOrder(a string) uint32 {
	ip, err := netip.ParseAddr(a)
	if err != nil || !ip.Is4() {
		return 0
	}
	b := ip.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// View builds the fleet table with LiteLLM health and script state.
func (s *Service) View(ctx context.Context, health HealthSource, mat Material) (FleetView, error) {
	peers, err := s.store.List(ctx)
	if err != nil {
		return FleetView{}, err
	}
	var h map[string]Health
	if health != nil {
		h = health(ctx)
	}
	now := s.now()
	stale := time.Duration(s.cfg.ReenrollSeconds*3) * time.Second
	sort.SliceStable(peers, func(i, j int) bool { return addrOrder(peers[i].Address) < addrOrder(peers[j].Address) })

	v := FleetView{Enabled: s.cfg.Enabled, Pool: s.cfg.Pool, EnrollHost: s.cfg.EnrollHostname, EnrollToken: s.cfg.EnrollToken,
		ReenrollSeconds: s.cfg.ReenrollSeconds, EndpointV6: s.wg.EndpointHostV6, Endpoint: s.wg.EndpointHost(), ListenPort: s.wg.ListenPort,
		AllowedSerials: len(s.cfg.AllowedSerials), Package: mat.Package(), PackageID: "de.dhbw.llm.fleet", Profiles: s.cfg.Profiles}
	if pool, err := netip.ParsePrefix(s.cfg.Pool); err == nil {
		v.Capacity = (1 << (32 - pool.Bits())) - 2
	}
	v.SelfUpdate.Mode, v.SelfUpdate.Canary, v.SelfUpdate.Scripts = UpdateMode(s.cfg), s.cfg.SelfUpdateCanary, s.scripts.Current()
	if v.SelfUpdate.Canary == nil {
		v.SelfUpdate.Canary = []string{}
	}

	for _, p := range peers {
		hp, known := h[p.Address]
		// "angehalten" first: the only WANTED state without inference.
		inference := "unbekannt"
		switch {
		case p.Busy:
			inference = "angehalten"
		case h == nil:
		case !known:
			inference = "nicht registriert"
		case hp.Bad == 0:
			inference = "erreichbar"
		case hp.OK == 0:
			inference = "gestört"
		default:
			inference = "teilweise"
		}
		serves := p.Models()
		modelState := "unbekannt"
		switch {
		case !p.Controls:
			modelState = "fremdverwaltet"
		case len(serves) == 0:
		case slices.Contains(serves, p.Model):
			modelState = "erfüllt"
		default:
			modelState = "abweichend"
		}
		state, quiet := "aktiv", 0
		if p.LastSeen != nil && now.Sub(*p.LastSeen) > stale {
			state, quiet = "ohne Rückmeldung", int(now.Sub(*p.LastSeen).Seconds())
		}
		if p.Blocked {
			state = "gesperrt"
		}
		transport := p.Transport
		if transport == "" {
			transport = "wireguard"
		}
		gate := p.Gate
		if gate == "" {
			gate = "idle-only"
		}
		v.Peers = append(v.Peers, PeerView{
			Serial: p.Serial, Name: p.Name, Address: p.Address, PrimaryIP: p.PrimaryIP, Transport: transport, APIBase: p.APIBase, OS: p.OS,
			Hostname: p.Hostname, Hardware: p.Hardware, RAMGb: p.RAMMb / 1024, Location: p.Location, Operator: p.Operator,
			Contact: p.Contact, Profile: p.Profile, Model: p.Model, Models: serves, ModelState: modelState,
			Serving: append([]string{}, hp.Models...), Inference: inference, InferenceError: hp.Error, Gate: gate,
			Enabled: p.Enabled, Busy: p.Busy, Controls: p.Controls, ScriptState: s.scripts.State(s.cfg, p),
			PkgVersion: p.PkgVersion, Canary: slices.Contains(s.cfg.SelfUpdateCanary, p.Serial), LastSeen: p.LastSeen,
			Blocked: p.Blocked, State: state, QuietSeconds: quiet,
		})
	}
	if v.Peers == nil {
		v.Peers = []PeerView{}
	}
	return v, nil
}

// CSV exports the fleet for inventory purposes.
func (s *Service) CSV(ctx context.Context) ([]byte, error) {
	peers, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"seriennummer", "name", "adresse", "primaere_ip", "klasse", "modell", "bietet_an", "ram_mb", "hostname",
		"hardware", "os", "standort", "betreuer", "kontakt", "gesperrt", "zuletzt_gemeldet"})
	for _, p := range peers {
		last := ""
		if p.LastSeen != nil {
			last = p.LastSeen.Format(time.RFC3339)
		}
		_ = w.Write([]string{p.Serial, p.Name, p.Address, p.PrimaryIP, p.Profile, p.Model, strings.Join(p.Models(), " "),
			strconv.Itoa(p.RAMMb), p.Hostname, p.Hardware, p.OSVersion, p.Location, p.Operator, p.Contact,
			strconv.FormatBool(p.Blocked), last})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// ---------------------------------------------------------------- import

var brokerTime = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d`)

// Import copies the broker's peers.json into the store (one-off migration).
// Existing serials are overwritten with the file's state.
func (s *Service) Import(ctx context.Context, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var file struct {
		Peers []map[string]any `json:"peers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	str := func(m map[string]any, k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(m map[string]any, k string) float64 {
		if v, ok := m[k].(float64); ok {
			return v
		}
		return 0
	}
	ts := func(m map[string]any, k string) *time.Time {
		v := str(m, k)
		if !brokerTime.MatchString(v) {
			return nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil
		}
		t = t.UTC()
		return &t
	}
	var peers []Peer
	for _, m := range file.Peers {
		p := Peer{
			Serial: str(m, "serial"), Name: str(m, "name"), Address: str(m, "address"), PublicKey: str(m, "publicKey"),
			PresharedKey: str(m, "presharedKey"), Transport: str(m, "transport"), APIBase: str(m, "apiBase"),
			APIKey: str(m, "apiKey"), Hostname: str(m, "hostname"), Hardware: str(m, "hardware"),
			OSVersion: str(m, "osVersion"), OS: str(m, "os"), GPU: str(m, "gpu"), Location: str(m, "location"),
			Operator: str(m, "operator"), Contact: str(m, "contact"), PrimaryIP: str(m, "primaryIp"), RAMMb: int(num(m, "ramMb")), Port: int(num(m, "port")),
			Controls: m["controls"] != false, Busy: m["busy"] == true, Scripts: str(m, "scripts"),
			PkgVersion: str(m, "pkgVersion"), Profile: str(m, "profile"), Model: str(m, "model"),
			Weight: num(m, "weight"), Gate: str(m, "gate"), Enabled: m["enabled"] != false, Blocked: m["blocked"] == true,
			LastSeen: ts(m, "lastSeen"),
		}
		if t := ts(m, "enrolledAt"); t != nil {
			p.EnrolledAt = *t
		}
		if list, ok := m["models"].([]any); ok {
			var ms []string
			for _, x := range list {
				ms = append(ms, fmt.Sprint(x))
			}
			p.SetModels(ms)
		}
		if p.Transport == "" {
			p.Transport = "wireguard"
		}
		if p.Serial == "" {
			continue
		}
		peers = append(peers, p)
	}
	err = s.store.Update(ctx, func([]Peer) ([]Peer, []string, error) { return peers, nil, nil })
	return len(peers), err
}
