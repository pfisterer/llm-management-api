package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
	// MachineSecret is set only in the answer that issues it; the machine keeps it and sends it as machineSecret from then on.
	MachineSecret string `json:"machineSecret,omitempty"`
	// Pending: the machine is recorded but awaits approval by a fleet admin; the answer carries no tunnel or assignment (the handler sends 202).
	Pending bool `json:"-"`
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
	extra   []ExtraSites
	size    ModelSize
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error) // DNS for the apiBase check (tests replace it)
}

// ExtraSites lists sites that are no enrolled machines but come and go like them (the GPU cluster's vLLM replicas). They are reported with Fleet=true, so the discovery job deregisters one as soon as it is no longer listed.
type ExtraSites func(ctx context.Context) ([]Site, error)

// AddSites adds a source of extra sites to Sites. An error of the source fails Sites as a whole: a partial list would make the discovery job deregister every site of that source.
func (s *Service) AddSites(src ExtraSites) { s.extra = append(s.extra, src) }

func NewService(cfg Config, wg WireGuard, store Store, scripts *Scripts) *Service {
	return &Service{cfg: cfg, wg: wg, store: store, scripts: scripts, now: func() time.Time { return time.Now().UTC() }, size: OllamaRegistry(&http.Client{}),
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}}
}

// SetModelSize replaces the lookup of a model's size (tests; nil = no check).
func (s *Service) SetModelSize(f ModelSize) { s.size = f }

func (s *Service) Config() Config       { return s.cfg }
func (s *Service) Scripts() *Scripts    { return s.scripts }
func (s *Service) WireGuard() WireGuard { return s.wg }

// maxPending caps the machines waiting for approval: every holder of the shared enrolment token can register new serials, and each one is a row in the fleet table.
const maxPending = 200

// Enroll registers or refreshes one machine. Idempotent on the serial number.
//
// The enrolment token is on every machine, so it says nothing about WHICH machine is calling. What does:
//   - A new serial starts Pending (and Blocked): it gets no tunnel address, no preshared key and no place in the peer or site list until a fleet admin unblocks it.
//   - A machine secret, handed out once (machineSecret in the answer), stored only as a hash and presented by the machine at every later enrolment. A known serial without the right secret is refused with 409; the admin reset is Forget, after which the machine enrols fresh (and waits for approval again).
//   - A known serial never changes its transport or its apiBase without that reset, and with the secret it may change its WireGuard key only together with a new preshared key: the old one is never handed to a different key.
//
// Migration (machines enrolled before the secret existed, and old scripts): a known serial without a stored secret is accepted only when it presents exactly the stored public key, transport and apiBase (trust on first use). The secret is issued only to a script that announces it can keep one, by sending the field machineSecret (empty at first); an old script would drop it and lock itself out at its next run. Such a machine keeps being recognised by its key alone until the self-update brings it the new script, which then picks up its secret at the next enrolment.
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
	apiBase := ""
	if transport == "direct" {
		pubkey = "" // no tunnel, no key: a stray one must neither identify nor claim anything
		apiBase = strings.TrimRight(req.str("apiBase"), "/")
		if !apiBaseRE.MatchString(apiBase) {
			return EnrollResponse{}, fail(400, "apiBase fehlt oder ist keine gueltige URL (transport=direct)")
		}
		if req.str("apiKey") == "" {
			return EnrollResponse{}, fail(400, "apiKey fehlt: ohne Tunnel schuetzt nur die eigene Anmeldung des Inferenzdienstes. Ein offener Endpunkt im Campusnetz ist nicht zulaessig.")
		}
		// Outside the registry lock: it may wait for DNS.
		if problem := s.apiBaseProblem(ctx, apiBase, true); problem != "" {
			return EnrollResponse{}, fail(400, "apiBase nicht zulaessig: %s", problem)
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
	// A script that sends the field (even empty) keeps the secret it is given; an older one does not.
	_, keepsSecret := req["machineSecret"]

	var out EnrollResponse
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		now := s.now()
		var mine *Peer
		pending := 0
		for i := range peers {
			if peers[i].Serial == serial {
				mine = &peers[i]
			}
			if peers[i].Pending {
				pending++
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
		existed := mine != nil
		secret := "" // a new machine secret, in this answer only
		if mine == nil {
			if pending >= maxPending {
				return nil, nil, fail(429, "too many machines awaiting approval")
			}
			peers = append(peers, Peer{Serial: serial, Transport: transport, APIBase: apiBase, PublicKey: pubkey, EnrolledAt: now, Controls: true, Blocked: true, Pending: true})
			mine = &peers[len(peers)-1]
		} else {
			if mine.Blocked && !mine.Pending {
				return nil, nil, fail(403, "machine is blocked")
			}
			if err := checkIdentity(*mine, req.str("machineSecret"), pubkey, transport, apiBase); err != nil {
				return nil, nil, err
			}
		}
		if mine.SecretHash == "" && keepsSecret {
			secret = genSecret()
			mine.SecretHash = hashSecret(secret)
		}
		// A new key (only possible with the secret): the old preshared key belongs to the old key and is not handed out again.
		if pubkey != mine.PublicKey {
			mine.PublicKey, mine.PresharedKey = pubkey, ""
		}
		mine.Transport = transport
		if transport == "direct" {
			mine.APIKey = truncate(req.str("apiKey"), 200)
		}
		// Tunnel material only for an approved machine, allocated at its first enrolment after the approval.
		if !mine.Blocked && transport != "direct" {
			if mine.Address == "" {
				if mine.Address = s.nextAddress(peers); mine.Address == "" {
					return nil, nil, fail(507, "fleet pool exhausted")
				}
			}
			if mine.PresharedKey == "" {
				mine.PresharedKey = genPSK()
			}
		}
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
		// The machine's own address in its local network (display only). Kept when the machine sends none or garbage: an older script must not blank it.
		if ip, err := netip.ParseAddr(req.str("primaryIp")); err == nil {
			mine.PrimaryIP = ip.String()
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
		// Why the machine could not pull or load its model, empty when it could; scripts from before send nothing and leave it.
		if v, ok := req["modelError"].(string); ok {
			mine.ModelError = truncate(strings.TrimSpace(v), 300)
		}

		a := s.cfg.assignment(profile, serial, mine.ModelOverride)
		mine.Profile, mine.Model, mine.Weight, mine.Gate, mine.Enabled = a.Profile, a.Model, a.Weight, a.Gate, a.Enabled
		mine.LastSeen = &now

		// Awaiting approval: recorded (the admin sees what is asking), nothing to configure yet.
		if mine.Pending {
			out = EnrollResponse{Name: mine.Name, Transport: mine.Transport, ReenrollSeconds: s.cfg.ReenrollSeconds, MachineSecret: secret, Pending: true}
			return []Peer{*mine}, nil, nil
		}
		out = EnrollResponse{
			Name: mine.Name, Address: mine.Address, PresharedKey: mine.PresharedKey, Transport: mine.Transport,
			MTU: 1420, Keepalive: 25, Port: s.cfg.Port, ReenrollSeconds: s.cfg.ReenrollSeconds,
			ScriptManifest: s.scripts.Manifest(s.cfg, serial),
			ScriptBaseURL:  "https://" + s.cfg.EnrollHostname + "/scripts/",
			OllamaVersion:  s.cfg.OllamaVersion,
			Profile: EnrollProfile{Name: a.Profile, Model: a.Model, WiredLimitMb: a.WiredLimitMb, Context: a.Context,
				Gate: a.Gate, Enabled: a.Enabled},
		}
		out.MachineSecret = secret
		// Tunnel fields stay empty (not absent) for direct machines: an agent
		// reading them gets "" and builds no tunnel instead of failing.
		if mine.Transport != "direct" {
			out.HubPublicKey = s.wg.HubPublicKey
			out.HubAddress = s.wg.HubAddress
			out.Endpoint = fmt.Sprintf("%s:%d", s.wg.EndpointHost(), s.wg.ListenPort)
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

// genSecret is a new machine secret: 32 random bytes, URL-safe base64 so it survives shell scripts and JSON unescaped.
func genSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// errReset is the answer to every enrolment that would take over a known serial: the shared token proves nothing about which machine calls.
func errReset(why string) error {
	return fail(409, "re-enrolment needs an admin reset (%s): delete the machine in the fleet admin page, it then enrols fresh and awaits approval", why)
}

// checkIdentity decides whether a request may speak for the known machine p (see Enroll for the rules and the migration).
func checkIdentity(p Peer, secret, pubkey, transport, apiBase string) error {
	if p.SecretHash != "" {
		want, _ := hex.DecodeString(p.SecretHash)
		got := sha256.Sum256([]byte(secret))
		if subtle.ConstantTimeCompare(want, got[:]) != 1 {
			return errReset("machine secret missing or wrong")
		}
	} else if pubkey != p.PublicKey {
		// No secret yet: the stored key is all that identifies the machine.
		return errReset("publicKey differs from the registered one")
	}
	stored := p.Transport
	if stored == "" {
		stored = "wireguard"
	}
	if transport != stored {
		return errReset("transport changes from " + stored + " to " + transport)
	}
	if transport == "direct" && apiBase != p.APIBase {
		return errReset("apiBase changes")
	}
	return nil
}

// notPublic are the ranges netip has no predicate for that must never be an inference endpoint either: "this network", carrier-grade NAT (100.64/10), IETF protocol assignments, benchmarking, reserved and broadcast, and NAT64 (which can embed any IPv4 address).
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
}

// publicAddr: an address LiteLLM may be sent to on a machine's say-so. Not loopback, private (RFC 1918 including the cluster's 10.42/10.43, ULA fc00::/7), link-local, CGNAT, multicast or unspecified.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// apiBaseProblem says why a direct machine's apiBase is not acceptable, "" when it is. https only: without the tunnel the site's API key and every prompt cross the network, and a plain-http site is reachable by anyone on the path. The host must be, or resolve only to, public addresses, so a machine cannot point LiteLLM at the cluster or the campus network behind it. A failed lookup is a problem at enrolment (strict) but not when the site list is built: a DNS hiccup there would deregister a working site, and the name was checked when it enrolled.
func (s *Service) apiBaseProblem(ctx context.Context, raw string, strict bool) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "eine https-URL ohne Zugangsdaten, Query und Fragment ist noetig"
	}
	host := u.Hostname()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		addrs, err = s.lookup(lctx, host)
		if err != nil || len(addrs) == 0 {
			if !strict {
				return ""
			}
			return fmt.Sprintf("%s laesst sich nicht aufloesen", host)
		}
	}
	for _, a := range addrs {
		if !publicAddr(a) {
			return fmt.Sprintf("%s zeigt auf %s, keine oeffentliche Adresse", host, a.Unmap())
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Forget removes a machine entirely (decommissioned device), and is the admin reset for re-enrolment: with the entry go the machine secret, the key, the transport and the apiBase. A machine that boots again enrols as a new one: it awaits approval and then gets a NEW address.
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

// ErrBadModel is a model name Ollama cannot have.
var ErrBadModel = errors.New("invalid model name")

var modelNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,95}$`)

// SetModel sets the model of the given machines (empty = back to their profile's). The assignment shown changes at once; a machine pulls the model at its next enrolment (reenrollSeconds). A model the Ollama registry does not have is refused (ErrUnknownModel), and so is one larger than the memory limit of any of the machines (*MisfitError) — a rough check, the machine's own load reports the rest. Unknown serials are skipped; ErrPeerNotFound when none is known.
func (s *Service) SetModel(ctx context.Context, serials []string, model string) (int, error) {
	model = strings.TrimSpace(model)
	if model != "" && !modelNameRE.MatchString(model) {
		return 0, ErrBadModel
	}
	size := int64(-1)
	if model != "" && s.size != nil {
		var err error
		if size, err = s.size(ctx, model); err != nil {
			return 0, err
		}
	}
	n := 0
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		var changed []Peer
		var misfits []string
		for _, p := range peers {
			if !slices.Contains(serials, p.Serial) {
				continue
			}
			profile, ok := s.cfg.profileFor(p.RAMMb)
			a := s.cfg.assignment(profile, p.Serial, model)
			if ok && size > 0 && a.WiredLimitMb > 0 && size/(1<<20)+kvReserveMb > int64(a.WiredLimitMb) {
				misfits = append(misfits, fmt.Sprintf("%s (%d GB)", p.Name, a.WiredLimitMb/1024))
				continue
			}
			p.ModelOverride = model
			p.ModelError = "" // a new attempt
			if ok {
				p.Model = a.Model
			} else if model != "" {
				p.Model = model
			}
			changed = append(changed, p)
		}
		if len(misfits) > 0 {
			return nil, nil, &MisfitError{Model: model, SizeGB: float64(size) / (1 << 30), Machines: misfits}
		}
		n = len(changed)
		return changed, nil, nil
	})
	if err == nil && n == 0 {
		return 0, ErrPeerNotFound
	}
	return n, err
}

// SetBlocked blocks or unblocks a machine. Unblocking a pending machine approves it: it gets its tunnel at its next enrolment. Either way it is no longer pending.
func (s *Service) SetBlocked(ctx context.Context, serial string, blocked bool) error {
	found := false
	err := s.store.Update(ctx, func(peers []Peer) ([]Peer, []string, error) {
		for _, p := range peers {
			if p.Serial == serial {
				found = true
				p.Blocked, p.Pending = blocked, false
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
