package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const key1 = "MQPtG7EOhnJh3azU3T1lmwx9A92KygkRcwH9n2j1zCc="
const key2 = "BkA4Tm1BshkuDPg5E2Lw//2lzCQVSubR8buKJlLHkhQ="
const key0 = "yg3IflBmWkteWyrjIJ0P/IIcqQAk6Wqb8VzJ5HLZUG0=" // ends in "0=": valid (broker lesson)

func testService(t *testing.T, mutate func(*Config)) (*Service, *MemoryStore, string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range fleetScripts {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n# "+n+" @@ENROLL_URL@@\n"), 0o644)
	}
	cfg, wg, err := ParseConfig(`{"enabled":true,"pool":"10.90.4.0/22","enrollHostname":"enroll.example","enrollToken":"et",
		"adminToken":"at","selfUpdate":true,"port":11434,"reenrollSeconds":3600,"apiKey":"",
		"litellmParams":{"extra_body":{"reasoning_effort":"none"}},
		"overrides":{"OFF00001":{"enabled":false}},
		"profiles":[{"name":"s","minRamMb":22000,"model":"small","wiredLimitMb":14336,"context":16384,"weight":1},
		            {"name":"l","minRamMb":46000,"model":"big","wiredLimitMb":30720,"context":32768,"weight":3}]}`,
		`{"hubPublicKey":"HUB=","listenPort":51820,"network":"10.90.0.0/16","hubAddress":"10.90.0.1","endpointHostV6":"wg6.example"}`)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&cfg)
	}
	st := NewMemoryStore()
	return NewService(cfg, wg, st, NewScripts(dir)), st, dir
}

func mac(serial, key, host string, ram int) EnrollRequest {
	return EnrollRequest{"serial": serial, "publicKey": key, "hostname": host, "ramMb": float64(ram),
		"osVersion": "26.7.1", "os": "darwin", "models": []any{"big"}, "scripts": "dhbw-llm-enroll.sh:" + strings.Repeat("a", 64)}
}

func status(err error) int {
	var e *EnrollError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestEnrollNewAndIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	r, err := svc.Enroll(ctx, mac("XL45R9YKW7", key1, "wimac02.local", 49152))
	if err != nil {
		t.Fatal(err)
	}
	if r.Address != "10.90.4.1" || r.Name != "wimac02" || r.Profile.Name != "l" || r.Profile.Model != "big" ||
		r.Endpoint != "wg6.example:51820" || r.HubPublicKey != "HUB=" || r.PresharedKey == "" ||
		r.ScriptBaseURL != "https://enroll.example/scripts/" || !strings.Contains(r.ScriptManifest, "dhbw-llm-enroll.sh:") {
		t.Fatalf("unexpected response: %+v", r)
	}
	r2, err := svc.Enroll(ctx, mac("XL45R9YKW7", key1, "wimac02", 49152))
	if err != nil || r2.Address != r.Address || r2.PresharedKey != r.PresharedKey {
		t.Fatalf("re-enrolment changed identity: %+v %v", r2, err)
	}
	peers, _ := st.List(ctx)
	if len(peers) != 1 || peers[0].LastSeen == nil || peers[0].Profile != "l" {
		t.Fatalf("registry: %+v", peers)
	}
}

func TestEnrollRules(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	if _, err := svc.Enroll(ctx, mac("A1", key1, "x", 49152)); status(err) != 400 {
		t.Fatalf("short serial: %v", err)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0001", "notakey", "x", 49152)); status(err) != 400 {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0001", key0, "x", 49152)); err != nil {
		t.Fatalf("key ending in 0= must be accepted: %v", err)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0002", key0, "y", 49152)); status(err) != 409 {
		t.Fatalf("key of another machine: %v", err)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0003", key2, "z", 8000)); status(err) != 400 {
		t.Fatalf("too little memory: %v", err)
	}
	if err := svc.SetBlocked(ctx, "SERIAL0001", true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0001", key0, "x", 49152)); status(err) != 403 {
		t.Fatalf("blocked machine: %v", err)
	}
	// Direct transport: API key mandatory, no tunnel address.
	direct := EnrollRequest{"serial": "LINUXBOX01", "transport": "direct", "apiBase": "https://gpu.example/v1/", "ramMb": 64000.0, "contact": "it@dhbw.de", "controls": false}
	if _, err := svc.Enroll(ctx, direct); status(err) != 400 {
		t.Fatalf("direct without api key: %v", err)
	}
	direct["apiKey"] = "secret"
	r, err := svc.Enroll(ctx, direct)
	if err != nil || r.Address != "" || r.Endpoint != "" || r.Transport != "direct" {
		t.Fatalf("direct: %+v %v", r, err)
	}
	// Foreign hardware needs a contact on first enrolment.
	if _, err := svc.Enroll(ctx, EnrollRequest{"serial": "LINUXBOX02", "publicKey": key2, "ramMb": 64000.0, "controls": false}); status(err) != 400 {
		t.Fatalf("foreign hardware without contact: %v", err)
	}
}

func TestNamesAndPool(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, func(c *Config) { c.Pool = "10.90.4.0/30" }) // two host addresses
	a, _ := svc.Enroll(ctx, mac("SERIAL0001", key1, "wimac", 49152))
	b, _ := svc.Enroll(ctx, mac("SERIAL0002", key2, "WIMAC", 49152))
	if a.Name != "wimac" || b.Name != "wimac-2" || a.Address != "10.90.4.1" || b.Address != "10.90.4.2" {
		t.Fatalf("names/addresses: %+v %+v", a, b)
	}
	if _, err := svc.Enroll(ctx, mac("SERIAL0003", key0, "c", 49152)); status(err) != 507 {
		t.Fatalf("pool exhaustion: %v", err)
	}
}

func TestSitesHubAndModels(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	_, _ = svc.Enroll(ctx, mac("SERIAL0001", key1, "ok", 49152))
	busy := mac("SERIAL0002", key2, "busy", 49152)
	busy["busy"] = true
	_, _ = svc.Enroll(ctx, busy)
	_, _ = svc.Enroll(ctx, mac("OFF00001", key0, "off", 49152)) // override enabled:false
	_, _ = svc.Enroll(ctx, EnrollRequest{"serial": "LINUXBOX01", "transport": "direct", "apiBase": "https://gpu.example/v1", "apiKey": "k", "ramMb": 64000.0})

	sites, _ := svc.Sites(ctx)
	names := map[string]Site{}
	for _, s := range sites {
		names[s.Name] = s
	}
	if _, ok := names["ok"]; !ok || len(sites) != 2 {
		t.Fatalf("sites: %+v", sites)
	}
	if names["ok"].Port != 11434 || names["ok"].WGAddress == "" || names["ok"].Weight != 3 || names["ok"].LiteLLMParams["extra_body"] == nil {
		t.Fatalf("site shape: %+v", names["ok"])
	}
	hub, _ := svc.HubPeers(ctx)
	if n := len(hub["peers"].([]HubPeer)); n != 3 {
		t.Fatalf("hub peers must exclude direct machines: %d", n)
	}
	// An empty model list does not overwrite what was reported before.
	empty := mac("SERIAL0001", key1, "ok", 49152)
	empty["models"] = []any{}
	_, _ = svc.Enroll(ctx, empty)
	peers, _ := st.List(ctx)
	for _, p := range peers {
		if p.Serial == "SERIAL0001" && len(p.Models()) != 1 {
			t.Fatalf("models overwritten: %v", p.Models())
		}
	}
}

func TestExtraSites(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	_, _ = svc.Enroll(ctx, mac("SERIAL0001", key1, "ok", 49152))
	svc.AddSites(func(context.Context) ([]Site, error) {
		return []Site{{Name: "gpu-gap-node", Weight: 1, Transport: "direct", APIBase: "https://inference.example/gap-node/v1", APIKey: "k"}}, nil
	})
	sites, err := svc.Sites(ctx)
	if err != nil || len(sites) != 2 {
		t.Fatalf("sites: %+v %v", sites, err)
	}
	// Reported like fleet machines, so the discovery job deregisters a replica that is gone.
	if g := sites[1]; g.Name != "gpu-gap-node" || !g.Fleet || g.LiteLLMParams["extra_body"] == nil {
		t.Fatalf("extra site: %+v", g)
	}
	// A failing source fails the whole list instead of returning one without it.
	svc.AddSites(func(context.Context) ([]Site, error) { return nil, errors.New("kubernetes down") })
	if _, err := svc.Sites(ctx); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPrimaryIP(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	view := func() PeerView {
		v, err := svc.View(ctx, nil, Material{})
		if err != nil || len(v.Peers) != 1 {
			t.Fatalf("view: %+v %v", v.Peers, err)
		}
		return v.Peers[0]
	}
	m := mac("SERIAL0001", key1, "ok", 49152)
	m["primaryIp"] = "141.72.16.102"
	_, _ = svc.Enroll(ctx, m)
	if p := view(); p.PrimaryIP != "141.72.16.102" {
		t.Fatalf("primary ip: %q", p.PrimaryIP)
	}
	// An older script sends none, a broken one garbage: the known address stays.
	for _, v := range []any{nil, "", "not-an-ip"} {
		m := mac("SERIAL0001", key1, "ok", 49152)
		if v != nil {
			m["primaryIp"] = v
		}
		_, _ = svc.Enroll(ctx, m)
		if p := view(); p.PrimaryIP != "141.72.16.102" {
			t.Fatalf("after %v: %q", v, p.PrimaryIP)
		}
	}
}

func TestScriptsAndCanary(t *testing.T) {
	svc, _, _ := testService(t, func(c *Config) { c.SelfUpdateCanary = []string{"CANARY0001"} })
	if m := svc.scripts.Manifest(svc.cfg, "OTHER00001"); m != "" {
		t.Fatalf("non-canary must get no manifest: %q", m)
	}
	if m := svc.scripts.Manifest(svc.cfg, "CANARY0001"); strings.Count(m, ":") != 3 {
		t.Fatalf("canary manifest: %q", m)
	}
	b, err := svc.scripts.Read(svc.cfg, "dhbw-llm-agent.sh")
	if err != nil || !strings.Contains(string(b), "https://enroll.example") {
		t.Fatalf("agent url not filled in: %q %v", b, err)
	}
	if _, err := svc.scripts.Read(svc.cfg, "../../etc/passwd"); !errors.Is(err, ErrScriptNotFound) {
		t.Fatalf("path traversal: %v", err)
	}
}

func TestImportFromBroker(t *testing.T) {
	ctx := context.Background()
	svc, st, dir := testService(t, nil)
	file := filepath.Join(dir, "peers.json")
	_ = os.WriteFile(file, []byte(`{"version":1,"peers":[
	 {"address":"10.90.4.1","blocked":false,"enabled":true,"enrolledAt":"2026-07-30T13:07:37+00:00","gate":"idle-only",
	  "hostname":"wimac02","lastSeen":"2026-10-06T06:56:24+00:00","model":"big","name":"wimac02","profile":"l",
	  "ramMb":49152,"serial":"XL45R9YKW7","weight":3,"transport":"wireguard","os":"darwin","models":["big"],
	  "controls":true,"busy":false,"publicKey":"`+key1+`","presharedKey":"psk="},
	 {"address":"10.90.4.2","serial":"ZWR-KI-VLLM-LARGE","name":"ki-vllm-large","ramMb":32094,"os":"linux",
	  "models":["Qwen3.6-27B"],"controls":false,"weight":2,"enabled":true,"publicKey":"`+key2+`"}]}`), 0o644)
	n, err := svc.Import(ctx, file)
	if err != nil || n != 2 {
		t.Fatalf("import: %d %v", n, err)
	}
	peers, _ := st.List(ctx) // sorted by serial: XL… before ZWR…
	p := peers[0]
	if p.Serial != "XL45R9YKW7" || p.LastSeen == nil || !p.LastSeen.Equal(time.Date(2026, 10, 6, 6, 56, 24, 0, time.UTC)) ||
		p.PresharedKey != "psk=" || p.Models()[0] != "big" || !p.Controls {
		t.Fatalf("imported peer: %+v", p)
	}
	if peers[1].Controls || peers[1].Transport != "wireguard" {
		t.Fatalf("linux peer: %+v", peers[1])
	}
	// The imported machine re-enrols and keeps its address.
	r, err := svc.Enroll(ctx, mac("XL45R9YKW7", key1, "wimac02", 49152))
	if err != nil || r.Address != "10.90.4.1" || r.PresharedKey != "psk=" {
		t.Fatalf("re-enrol after import: %+v %v", r, err)
	}
}

func TestMobileconfig(t *testing.T) {
	svc, _, dir := testService(t, nil)
	tmpl := filepath.Join(dir, "p.tmpl")
	_ = os.WriteFile(tmpl, []byte("<url>__ENROLL_URL__</url><tok>__ENROLL_TOKEN__</tok><u>__UUID_TOP__</u><i>__UUID_INNER__</i>\n    <!-- __OPTIONAL_KEYS__ -->\n</dict>"), 0o644)
	m := Material{ProfileTemplate: tmpl}
	a, err := m.Mobileconfig(svc.cfg, ProfileOptions{Location: "DHBW <MA>"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Mobileconfig(svc.cfg, ProfileOptions{Location: "DHBW <MA>"})
	s := string(a)
	if string(a) != string(b) || !strings.Contains(s, "https://enroll.example") || !strings.Contains(s, "<tok>et</tok>") ||
		!strings.Contains(s, "DHBW &lt;MA&gt;") || strings.Contains(s, "__") {
		t.Fatalf("mobileconfig:\n%s", s)
	}
}
