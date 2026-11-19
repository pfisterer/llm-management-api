package fleet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A strata model is set like any model, but it is no Ollama model: no registry lookup, no fit against the wired limit; the machine must be a Mac of the fleet in the tunnel with enough memory instead.
func TestSetStrataModel(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, func(c *Config) { c.APIKey = "fleet-key" })
	asked := 0
	svc.SetModelSize(func(context.Context, string) (int64, error) { asked++; return 0, ErrUnknownModel })
	join(t, svc, mac("SERIAL0001", key1, "big", 49152))
	join(t, svc, mac("SERIAL0002", key2, "small", 32768))
	linux := mac("LINUX0001", key0, "box", 131072)
	linux["os"], linux["osVersion"] = "linux", "Ubuntu 24.04"
	join(t, svc, linux)

	if _, err := svc.SetModel(ctx, []string{"SERIAL0001"}, "strata:nope"); !errors.Is(err, ErrUnknownStrata) {
		t.Errorf("unknown strata name accepted: %v", err)
	}
	// Too small and not a Mac: nothing is set, both are named.
	var misfit *StrataMisfitError
	if _, err := svc.SetModel(ctx, []string{"SERIAL0001", "SERIAL0002", "LINUX0001"}, "strata:gyro-s"); !errors.As(err, &misfit) ||
		len(misfit.Machines) != 2 || !slices.Contains(misfit.Machines, "small (32 GB)") || !slices.Contains(misfit.Machines, "box (linux)") || misfit.MinRAMGb != 48 {
		t.Fatalf("misfit: %+v %v", misfit, err)
	}
	if peers, _ := st.List(ctx); peers[0].ModelOverride != "" {
		t.Errorf("set despite a misfit")
	}
	if n, err := svc.SetModel(ctx, []string{"SERIAL0001"}, "strata:gyro-s"); err != nil || n != 1 {
		t.Fatalf("set: %d %v", n, err)
	}
	if asked != 0 {
		t.Errorf("the Ollama registry was asked for a strata model")
	}

	// The enrolment hands the name through unchanged, with the strata context and the machine's own key.
	r, err := svc.Enroll(ctx, mac("SERIAL0001", key1, "big", 49152))
	if err != nil || r.Profile.Model != "strata:gyro-s" || r.Profile.Context != 32768 || r.Profile.Name != "l" {
		t.Fatalf("enrolment: %+v %v", r.Profile, err)
	}
	key := svc.cfg.strataKey("SERIAL0001")
	if r.APIKey != key || !strings.HasPrefix(key, "strata-") || len(key) != len("strata-")+64 || strings.Contains(key, "fleet-key") ||
		key == svc.cfg.strataKey("SERIAL0002") {
		t.Errorf("api key: %q", r.APIKey)
	}
	// A machine with an Ollama model gets no key.
	if r, _ := svc.Enroll(ctx, mac("SERIAL0002", key2, "small", 32768)); r.APIKey != "" {
		t.Errorf("ollama machine got a key")
	}

	// Discovery: the machine's own key, only the strata alias, low weight, strata context; the others as before.
	sites, err := svc.Sites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Site{}
	for _, s := range sites {
		byName[s.Name] = s
	}
	if s := byName["big"]; s.APIKey != key || len(s.Aliases) != 1 || s.Aliases[0] != "chat-large" || s.Weight != 0.5 || s.Context != 32768 {
		t.Errorf("strata site: %+v", s)
	}
	if s := byName["small"]; s.APIKey != "fleet-key" || s.Aliases != nil || s.Context != 16384 {
		t.Errorf("ollama site: %+v", s)
	}

	// The view offers the names; the machine reports the name as served once Strata answers.
	served := mac("SERIAL0001", key1, "big", 49152)
	served["models"] = []any{"strata:gyro-s"}
	_, _ = svc.Enroll(ctx, served)
	v, err := svc.View(ctx, nil, Material{})
	if err != nil || len(v.StrataModels) != 1 || v.StrataModels[0] != "strata:gyro-s" {
		t.Fatalf("view: %v %v", v.StrataModels, err)
	}
	for _, p := range v.Peers {
		if p.Name == "big" && (p.Model != "strata:gyro-s" || p.ModelState != "erfüllt") {
			t.Errorf("view peer: %+v", p)
		}
	}

	// Back to Ollama: the profile's model, context and weight, no key.
	if _, err := svc.SetModel(ctx, []string{"SERIAL0001"}, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = svc.Enroll(ctx, mac("SERIAL0001", key1, "big", 49152))
	if r.Profile.Model != "big" || r.Profile.Context != 32768 || r.APIKey != "" {
		t.Errorf("back to ollama: %+v %q", r.Profile, r.APIKey)
	}
	sites, _ = svc.Sites(ctx)
	for _, s := range sites {
		if s.Name == "big" && (s.Aliases != nil || s.Weight != 3 || s.APIKey != "fleet-key") {
			t.Errorf("site after switching back: %+v", s)
		}
	}
}

// Strata settings and the inventory's override: the override's weight wins, its context (Ollama's) does not; an unknown strata name in an override fails at start.
func TestStrataAssignment(t *testing.T) {
	w, ctxOllama := 1.5, 65536
	strata := "strata:gyro-s"
	c := Config{Overrides: map[string]Override{"A": {Model: &strata, Weight: &w, Context: &ctxOllama}, "B": {Model: &strata}},
		Strata: StrataConfig{Context: 16384}}
	p := Profile{Name: "l", Model: "big", Context: 32768, Weight: 3}
	if a := c.assignment(p, "A", ""); a.Model != strata || a.Weight != 1.5 || a.Context != 16384 {
		t.Errorf("A: %+v", a)
	}
	if a := c.assignment(p, "B", ""); a.Weight != 0.5 || a.Context != 16384 {
		t.Errorf("B: %+v", a)
	}
	// The portal's model wins over the override's strata model.
	if a := c.assignment(p, "B", "qwen3.5:9b"); a.Model != "qwen3.5:9b" || a.Weight != 3 || a.Context != 32768 {
		t.Errorf("B with portal model: %+v", a)
	}
	if _, _, err := ParseConfig(`{"overrides":{"X":{"model":"strata:typo"}}}`, ""); err == nil || !strings.Contains(err.Error(), "strata:gyro-s") {
		t.Errorf("unknown strata override accepted: %v", err)
	}
	if (Config{}).strataKey("A") != "" {
		t.Errorf("a key without a fleet key")
	}
}

// strataPeer is a Mac with strata:gyro-s assigned, seen at seen; has = it reports the model as served.
func strataPeer(serial, name, ip string, seen time.Time, has bool) Peer {
	p := Peer{Serial: serial, Name: name, PrimaryIP: ip, Transport: "wireguard", Model: "strata:gyro-s", LastSeen: &seen}
	if has {
		p.SetModels([]string{"strata:gyro-s"})
	}
	return p
}

// The source of the model download comes from the fleet's state alone: a Mac of the same network that has the model and is online, not blocked, pending or busy, with the same strata model; spread per requester; otherwise the first by name downloads and the others wait.
func TestStrataSource(t *testing.T) {
	svc, _, _ := testService(t, nil)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-50*time.Minute), now.Add(-2*time.Hour)
	me := strataPeer("ME000001", "wimac10", "141.72.10.50", now, false)
	src := func(peers ...Peer) *EnrollStrata { return svc.strataSource(append(peers, me), me, now) }

	// Nobody else: this Mac leads and fetches from Hugging Face; its own network and port come along.
	if r := src(); r == nil || r.Source != "" || r.Wait || r.Network != "141.72.10.0/24" || r.Port != 11480 || r.WaitHours != 6 {
		t.Fatalf("alone: %+v", r)
	}
	// A ready Mac in the same /24.
	ready := strataPeer("SRC00001", "wimac20", "141.72.10.20", recent, true)
	if r := src(ready); r.Source != "http://141.72.10.20:11480" || r.Wait {
		t.Errorf("same network: %+v", r)
	}
	// Never across networks, whatever else holds: the other one leads there, here this Mac leads.
	other := strataPeer("SRC00002", "wimac01", "141.72.11.20", recent, true)
	if r := src(other); r.Source != "" || r.Wait {
		t.Errorf("other network: %+v", r)
	}
	// Excluded: offline, blocked, pending, another model, not ready, busy (the last two make it wait or lead, never a source).
	offline := strataPeer("SRC00003", "wimac21", "141.72.10.21", old, true)
	blocked := strataPeer("SRC00004", "wimac22", "141.72.10.22", recent, true)
	blocked.Blocked = true
	pending := strataPeer("SRC00005", "wimac23", "141.72.10.23", recent, true)
	pending.Pending, pending.Blocked = true, true
	ollama := strataPeer("SRC00006", "wimac24", "141.72.10.24", recent, false)
	ollama.Model = "big"
	ollama.SetModels([]string{"big"})
	direct := strataPeer("SRC00007", "wimac25", "141.72.10.25", recent, true)
	direct.Transport = "direct"
	if r := src(offline, blocked, pending, ollama, direct); r.Source != "" || r.Wait {
		t.Errorf("excluded peers: %+v", r)
	}
	busy := strataPeer("SRC00008", "wimac26", "141.72.10.26", recent, true)
	busy.Busy = true
	if r := src(busy); r.Source != "" || !r.Wait {
		t.Errorf("only a busy Mac has it: wait for it, %+v", r)
	}
	if r := src(busy, ready); r.Source != "http://141.72.10.20:11480" {
		t.Errorf("busy and ready: %+v", r)
	}

	// Leader: the first by name of the online Macs of the network without the model downloads, the others wait; when it drops out, the next one leads.
	lead := strataPeer("LEAD0001", "wimac05", "141.72.10.5", recent, false)
	if r := src(lead); r.Source != "" || !r.Wait {
		t.Errorf("not the leader: %+v", r)
	}
	later := strataPeer("LATE0001", "wimac30", "141.72.10.30", recent, false)
	if r := src(later); r.Wait {
		t.Errorf("leader: %+v", r)
	}
	lead.LastSeen = &old
	if r := src(lead, later); r.Wait {
		t.Errorf("leader gone, this Mac takes over: %+v", r)
	}
	lead.LastSeen = &recent
	lead.Blocked = true
	if r := src(lead, later); r.Wait {
		t.Errorf("leader blocked, this Mac takes over: %+v", r)
	}
	// A leader in another network does not count.
	far := strataPeer("FAR00001", "wimac00", "141.72.12.5", recent, false)
	if r := src(far); r.Wait {
		t.Errorf("leader in another network: %+v", r)
	}

	// A Mac that has the model itself needs no source; one without IPv4 takes part in nothing.
	has := me
	has.SetModels([]string{"strata:gyro-s"})
	if r := svc.strataSource([]Peer{ready, has}, has, now); r.Source != "" || r.Wait || r.Network == "" {
		t.Errorf("has it: %+v", r)
	}
	v6 := me
	v6.PrimaryIP = "2001:7c7:2000::50"
	if r := svc.strataSource([]Peer{ready, lead, v6}, v6, now); r.Source != "" || r.Wait || r.Network != "" || r.Port != 11480 {
		t.Errorf("ipv6: %+v", r)
	}
	// An Ollama machine gets no strata part at all.
	if r := svc.strataSource(nil, ollama, now); r != nil {
		t.Errorf("ollama: %+v", r)
	}
}

// Spreading: every requester keeps its source while the set is stable, different requesters use different sources, and a source that leaves only moves the requesters it served.
func TestStrataSourceSpread(t *testing.T) {
	svc, _, _ := testService(t, func(c *Config) { c.Strata = StrataConfig{SourcePrefix: 16, SourcePort: 12000, SourceWaitHours: 2} })
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var sources []Peer
	for i := range 4 {
		sources = append(sources, strataPeer(fmt.Sprintf("SRC0000%d", i), fmt.Sprintf("src%d", i), fmt.Sprintf("10.20.%d.1", i), now, true))
	}
	pick := func(set []Peer, serial string) string {
		me := strataPeer(serial, "req-"+serial, "10.20.200.9", now, false)
		r := svc.strataSource(append(slices.Clone(set), me), me, now)
		if r.Network != "10.20.0.0/16" || r.WaitHours != 2 {
			t.Fatalf("config: %+v", r)
		}
		return r.Source
	}
	rev := slices.Clone(sources)
	slices.Reverse(rev)
	used := map[string]int{}
	first := map[string]string{}
	for i := range 40 {
		serial := fmt.Sprintf("REQ%05d", i)
		s := pick(sources, serial)
		if !strings.HasSuffix(s, ":12000") {
			t.Fatalf("port: %s", s)
		}
		// Same answer again, and with the sources in another order.
		if again := pick(rev, serial); again != s {
			t.Fatalf("not deterministic for %s: %s then %s", serial, s, again)
		}
		used[s]++
		first[serial] = s
	}
	if len(used) != 4 {
		t.Errorf("not spread over all sources: %v", used)
	}
	// src0 leaves: only its requesters move.
	gone := "http://10.20.0.1:12000"
	for serial, s := range first {
		if now := pick(sources[1:], serial); s != gone && now != s {
			t.Errorf("%s moved from %s to %s although its source stayed", serial, s, now)
		}
	}
}

// Through the enrolment: the answer of a strata machine carries the `strata` part, from the state the enrolments leave behind; an Ollama machine's does not.
func TestEnrollStrataSource(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, func(c *Config) { c.APIKey = "fleet-key" })
	a, b := mac("SERIAL0001", key1, "wimac01", 49152), mac("SERIAL0002", key2, "wimac02", 49152)
	a["primaryIp"], b["primaryIp"] = "141.72.10.1", "141.72.10.2"
	if r := join(t, svc, a); r.Strata != nil {
		t.Fatalf("ollama machine with a strata part: %+v", r.Strata)
	}
	join(t, svc, b)
	if _, err := svc.SetModel(ctx, []string{"SERIAL0001", "SERIAL0002"}, "strata:gyro-s"); err != nil {
		t.Fatal(err)
	}
	// While setting up, both report busy and no models.
	a["busy"], b["busy"] = true, true
	a["models"], b["models"] = []any{}, []any{}
	ra, _ := svc.Enroll(ctx, a)
	rb, _ := svc.Enroll(ctx, b)
	if ra.Strata == nil || ra.Strata.Wait || ra.Strata.Source != "" || ra.Strata.Network != "141.72.10.0/24" {
		t.Errorf("leader: %+v", ra.Strata)
	}
	if rb.Strata == nil || !rb.Strata.Wait {
		t.Errorf("follower: %+v", rb.Strata)
	}
	// The leader is ready: the follower fetches from it.
	a["busy"], a["models"] = false, []any{"strata:gyro-s"}
	if ra, _ = svc.Enroll(ctx, a); ra.Strata.Wait || ra.Strata.Source != "" {
		t.Errorf("leader ready: %+v", ra.Strata)
	}
	if rb, _ = svc.Enroll(ctx, b); rb.Strata.Wait || rb.Strata.Source != "http://141.72.10.1:11480" {
		t.Errorf("follower with a source: %+v", rb.Strata)
	}

	for _, bad := range []string{`{"strata":{"sourcePrefix":4}}`, `{"strata":{"sourcePrefix":33}}`, `{"strata":{"sourcePort":80}}`, `{"strata":{"sourceWaitHours":-1}}`} {
		if _, _, err := ParseConfig(bad, ""); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}
