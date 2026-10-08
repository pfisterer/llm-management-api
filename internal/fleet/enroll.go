package fleet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	serialRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{3,31}$`)
	// 32 bytes base64: the last character carries four bits only, so exactly the
	// 16 characters whose index is divisible by four are possible. Without the
	// digits 0, 4, 8 about every fifth machine was refused (lesson from the broker).
	wgKeyRE   = regexp.MustCompile(`^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]=$`)
	contactRE = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]{2,}$`)
	scriptRE  = regexp.MustCompile(`^[\w.-]{1,40}:[0-9a-f]{64}$`)
	apiBaseRE = regexp.MustCompile(`^https?://[^\s/]+(/\S*)?$`)
	nameClean = regexp.MustCompile(`[^a-z0-9-]+`)
	osLinux   = regexp.MustCompile(`(?i)ubuntu|debian|linux|rocky|alma|fedora|suse`)
	osDarwin  = regexp.MustCompile(`^\d+\.\d`)
)

// EnrollRequest is what the agent sends. Loosely typed on purpose: older
// agents send fewer fields or numbers as strings.
type EnrollRequest map[string]any

func (r EnrollRequest) str(k string) string {
	switch v := r[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func (r EnrollRequest) int(k string) int {
	n, _ := strconv.Atoi(strings.SplitN(r.str(k), ".", 2)[0])
	return n
}

// EnrollProfile is the assignment part of the answer.
type EnrollProfile struct {
	Name         string `json:"name"`
	Model        string `json:"model"`
	WiredLimitMb int    `json:"wiredLimitMb"`
	Context      int    `json:"context"`
	Gate         string `json:"gate"`
	Enabled      bool   `json:"enabled"`
}

// EnrollResponse is everything a machine needs for wg0.conf and its service.
// Field names are the broker's: the shell agents read them with plutil.
type EnrollResponse struct {
	Name            string        `json:"name"`
	Address         string        `json:"address"`
	PresharedKey    string        `json:"presharedKey"`
	Transport       string        `json:"transport"`
	HubPublicKey    string        `json:"hubPublicKey"`
	HubAddress      string        `json:"hubAddress"`
	Endpoint        string        `json:"endpoint"`
	MTU             int           `json:"mtu"`
	Keepalive       int           `json:"keepalive"`
	Port            int           `json:"port"`
	ReenrollSeconds int           `json:"reenrollSeconds"`
	ScriptManifest  string        `json:"scriptManifest"`
	ScriptBaseURL   string        `json:"scriptBaseUrl"`
	OllamaVersion   string        `json:"ollamaVersion"`
	Profile         EnrollProfile `json:"profile"`
}

// EnrollError carries the HTTP status the broker used for the case.
type EnrollError struct {
	Status  int
	Message string
}

func (e *EnrollError) Error() string { return e.Message }

func fail(status int, format string, a ...any) error {
	return &EnrollError{Status: status, Message: fmt.Sprintf(format, a...)}
}

// Service holds the fleet logic.
type Service struct {
	cfg     Config
	wg      WireGuard
	store   Store
	scripts *Scripts
	now     func() time.Time
}

func NewService(cfg Config, wg WireGuard, store Store, scripts *Scripts) *Service {
	return &Service{cfg: cfg, wg: wg, store: store, scripts: scripts, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Config() Config       { return s.cfg }
func (s *Service) Scripts() *Scripts    { return s.scripts }
func (s *Service) WireGuard() WireGuard { return s.wg }

// Enroll registers or refreshes one machine. Idempotent on the serial number.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (EnrollResponse, error) {
	if !s.cfg.Enabled {
		return EnrollResponse{}, fail(503, "fleet enrollment disabled")
	}
	serial := req.str("serial")
	pubkey := req.str("publicKey")
	if !serialRE.MatchString(serial) {
		return EnrollResponse{}, fail(400, "invalid serial")
	}
	// The tunnel is the norm, not a requirement. Without it the inference
	// service's own API key is the only protection, so it is mandatory then.
	transport := "wireguard"
	if req.str("transport") == "direct" {
		transport = "direct"
	}
	apiBase := strings.TrimRight(req.str("apiBase"), "/")
	if transport == "direct" {
		if !apiBaseRE.MatchString(apiBase) {
			return EnrollResponse{}, fail(400, "apiBase fehlt oder ist keine gueltige URL (transport=direct)")
		}
		if req.str("apiKey") == "" {
			return EnrollResponse{}, fail(400, "apiKey fehlt: ohne Tunnel schuetzt nur die eigene Anmeldung des Inferenzdienstes. Ein offener Endpunkt im Campusnetz ist nicht zulaessig.")
		}
	} else if !wgKeyRE.MatchString(pubkey) {
		return EnrollResponse{}, fail(400, "invalid publicKey (expected 44-char base64 X25519 key)")
	}
	if len(s.cfg.AllowedSerials) > 0 && !slices.Contains(s.cfg.AllowedSerials, serial) {
		return EnrollResponse{}, fail(403, "serial not in allowedSerials")
	}
	ramMb := req.int("ramMb")
	profile, ok := s.cfg.profileFor(ramMb)
	if !ok {
		return EnrollResponse{}, fail(400, "no profile for %d MB of memory — too small for this service", ramMb)
	}

	var out EnrollResponse
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		now := s.now()
		var mine *Peer
		for i := range peers {
			if peers[i].Serial == serial {
				mine = &peers[i]
			}
		}
		// A key claimed by a DIFFERENT machine is refused, not moved: a cloned
		// disk or a replayed key; reassigning would break the legitimate owner.
		if pubkey != "" {
			for _, p := range peers {
				if p.PublicKey == pubkey && p.Serial != serial {
					return nil, nil, fail(409, "publicKey already registered for %s", p.Serial)
				}
			}
		}
		if mine != nil && mine.Blocked {
			return nil, nil, fail(403, "machine is blocked")
		}
		existed := mine != nil
		if mine == nil {
			address, psk := "", ""
			if transport != "direct" {
				address = s.nextAddress(peers)
				if address == "" {
					return nil, nil, fail(507, "fleet pool exhausted")
				}
				psk = genPSK()
			}
			peers = append(peers, Peer{Serial: serial, Address: address, PresharedKey: psk, EnrolledAt: now, Controls: true})
			mine = &peers[len(peers)-1]
		}

		mine.Transport = transport
		if transport == "direct" {
			mine.APIBase = apiBase
			mine.APIKey = truncate(req.str("apiKey"), 200)
		}
		mine.PublicKey = pubkey
		// Descriptive fields: only overwrite what the machine actually sent.
		for _, f := range []struct {
			key   string
			limit int
			dst   *string
		}{
			{"hostname", 64, &mine.Hostname}, {"hardware", 32, &mine.Hardware}, {"osVersion", 32, &mine.OSVersion},
			{"location", 64, &mine.Location}, {"operator", 64, &mine.Operator}, {"gpu", 48, &mine.GPU},
			{"os", 16, &mine.OS}, {"contact", 96, &mine.Contact},
		} {
			if v := truncate(req.str(f.key), f.limit); v != "" || !existed {
				*f.dst = v
			}
		}
		if mine.Contact != "" && !contactRE.MatchString(mine.Contact) {
			return nil, nil, fail(400, "contact ist keine gueltige Adresse: %s", mine.Contact)
		}
		controls := req["controls"] != false && req.str("controls") != "false"
		// A technical contact is required for hardware we do not control, and
		// only at the FIRST enrolment: refusing a running site later would switch
		// it off to enforce a data gap.
		if !controls && mine.Contact == "" && !existed {
			return nil, nil, fail(400, "contact fehlt: ein Rechner, dessen Inferenzdienst wir nicht steuern, braucht eine technische Kontaktadresse (Installation mit --contact).")
		}
		if mine.OS == "" {
			mine.OS = osFrom(mine.OSVersion)
		}
		mine.Name = peerName(*mine, peers)
		mine.RAMMb = ramMb
		if port := req.int("port"); port > 0 && port < 65536 {
			mine.Port = port
		}
		// What the machine really serves. An empty list overwrites nothing: a
		// stopped service cannot be asked, and "nothing" would be the wrong answer.
		if raw, ok := req["models"].([]any); ok {
			seen := map[string]bool{}
			var models []string
			for _, m := range raw {
				name := truncate(strings.TrimSpace(fmt.Sprint(m)), 96)
				if name != "" && !seen[name] && len(models) < 40 && !strings.ContainsAny(name, " \t") {
					seen[name] = true
					models = append(models, name)
				}
			}
			slices.Sort(models)
			if len(models) > 0 || mine.ModelsList == "" {
				mine.SetModels(models)
			}
		}
		mine.Controls = controls
		// Set by the gate while someone works at the machine: it then drops out
		// of the site list, and discovery removes its deployments.
		mine.Busy = req["busy"] == true || req.str("busy") == "true"
		if raw, ok := req["scripts"].(string); ok {
			var kept []string
			for _, e := range strings.Fields(raw) {
				if scriptRE.MatchString(e) && len(kept) < 10 {
					kept = append(kept, e)
				}
			}
			mine.Scripts = strings.Join(kept, " ")
		}
		if v, ok := req["pkgVersion"].(string); ok {
			mine.PkgVersion = truncate(v, 32)
		}

		a := s.cfg.assignment(profile, serial)
		mine.Profile, mine.Model, mine.Weight, mine.Gate, mine.Enabled = a.Profile, a.Model, a.Weight, a.Gate, a.Enabled
		mine.LastSeen = &now

		out = EnrollResponse{
			Name: mine.Name, Address: mine.Address, PresharedKey: mine.PresharedKey, Transport: mine.Transport,
			MTU: 1420, Keepalive: 25, Port: s.cfg.Port, ReenrollSeconds: s.cfg.ReenrollSeconds,
			ScriptManifest: s.scripts.Manifest(s.cfg, serial),
			ScriptBaseURL:  "https://" + s.cfg.EnrollHostname + "/scripts/",
			OllamaVersion:  s.cfg.OllamaVersion,
			Profile: EnrollProfile{Name: a.Profile, Model: a.Model, WiredLimitMb: a.WiredLimitMb, Context: a.Context,
				Gate: a.Gate, Enabled: a.Enabled},
		}
		// Tunnel fields stay empty (not absent) for direct machines: an agent
		// reading them gets "" and builds no tunnel instead of failing.
		if mine.Transport != "direct" {
			out.HubPublicKey = s.wg.HubPublicKey
			out.HubAddress = s.wg.HubAddress
			out.Endpoint = fmt.Sprintf("%s:%d", s.wg.EndpointHostV6, s.wg.ListenPort)
		}
		return []Peer{*mine}, nil, nil
	})
	return out, err
}

// nextAddress is the lowest free host address in the pool; both edge
// addresses are skipped by convention.
func (s *Service) nextAddress(peers []Peer) string {
	pool := netip.MustParsePrefix(s.cfg.Pool)
	taken := map[string]bool{}
	for _, p := range peers {
		taken[p.Address] = true
	}
	first := pool.Addr().Next()
	for a := first; pool.Contains(a); a = a.Next() {
		if !pool.Contains(a.Next()) {
			break // broadcast address
		}
		if !taken[a.String()] {
			return a.String()
		}
	}
	return ""
}

// peerName: a readable name from the hostname ("wimac02"), unique across the
// fleet. It is the site name in LiteLLM and therefore part of every deployment
// id, so it is cut hard.
func peerName(p Peer, peers []Peer) string {
	base := strings.ToLower(p.Hostname)
	for _, suf := range []string{".local", ".lan", ".intern", ".internal"} {
		base = strings.TrimSuffix(base, suf)
	}
	base = strings.Trim(nameClean.ReplaceAllString(base, "-"), "-")
	if len(base) > 40 {
		base = base[:40]
	}
	wanted := base
	if len(base) < 2 {
		wanted = "mac-" + strings.ToLower(p.Serial)
	}
	if p.Name == wanted {
		return p.Name
	}
	taken := map[string]bool{}
	for _, o := range peers {
		if o.Serial != p.Serial {
			taken[o.Name] = true
		}
	}
	if !taken[wanted] {
		return wanted
	}
	for n := 2; n < 100; n++ {
		if c := fmt.Sprintf("%s-%d", wanted, n); !taken[c] {
			return c
		}
	}
	return "mac-" + strings.ToLower(p.Serial)
}

func osFrom(osVersion string) string {
	switch {
	case osLinux.MatchString(osVersion):
		return "linux"
	case osDarwin.MatchString(osVersion):
		return "darwin"
	}
	return ""
}

func genPSK() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Forget removes a machine entirely (decommissioned device). Unlike blocking,
// a machine that boots again re-enrols and gets a NEW address.
func (s *Service) Forget(ctx context.Context, serial string) error {
	found := false
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		for _, p := range peers {
			if p.Serial == serial {
				found = true
				return nil, []string{serial}, nil
			}
		}
		return nil, nil, nil
	})
	if err == nil && !found {
		return ErrPeerNotFound
	}
	return err
}

// SetBlocked blocks or unblocks a machine.
func (s *Service) SetBlocked(ctx context.Context, serial string, blocked bool) error {
	found := false
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		for _, p := range peers {
			if p.Serial == serial {
				found = true
				p.Blocked = blocked
				return []Peer{p}, nil, nil
			}
		}
		return nil, nil, nil
	})
	if err == nil && !found {
		return ErrPeerNotFound
	}
	return err
}
