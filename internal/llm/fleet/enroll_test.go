package fleet

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newScript is a request from a script that keeps a machine secret (it sends the field, empty until it has one).
func newScript(req EnrollRequest, secret string) EnrollRequest {
	req["machineSecret"] = secret
	return req
}

func TestNewMachineAwaitsApproval(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	r, err := svc.Enroll(ctx, newScript(mac("NEWMAC0001", key1, "neu", 49152), ""))
	if err != nil || !r.Pending || r.Address != "" || r.PresharedKey != "" || r.HubPublicKey != "" || r.Endpoint != "" || r.MachineSecret == "" {
		t.Fatalf("pending answer: %+v %v", r, err)
	}
	hub, _ := svc.HubPeers(ctx)
	sites, _ := svc.Sites(ctx)
	if len(hub["peers"].([]HubPeer)) != 0 || len(sites) != 0 {
		t.Fatalf("pending machine listed: %+v %+v", hub, sites)
	}
	v, _ := svc.View(ctx, nil, Material{})
	if p := v.Peers[0]; !p.Pending || !p.Blocked || p.State != "wartet auf Freigabe" || p.Hostname != "neu" || !p.HasSecret {
		t.Fatalf("view: %+v", p)
	}
	// Asking again while pending: still nothing, and the secret is not handed out twice.
	if r2, err := svc.Enroll(ctx, newScript(mac("NEWMAC0001", key1, "neu", 49152), r.MachineSecret)); err != nil || !r2.Pending || r2.MachineSecret != "" || r2.PresharedKey != "" {
		t.Fatalf("pending again: %+v %v", r2, err)
	}
	// The secret is stored as a hash only.
	if peers, _ := st.List(ctx); peers[0].SecretHash == r.MachineSecret || peers[0].SecretHash != hashSecret(r.MachineSecret) {
		t.Fatalf("secret stored as is")
	}
	// Approval = unblock; the tunnel comes with the next enrolment.
	if err := svc.SetBlocked(ctx, "NEWMAC0001", false); err != nil {
		t.Fatal(err)
	}
	r3, err := svc.Enroll(ctx, newScript(mac("NEWMAC0001", key1, "neu", 49152), r.MachineSecret))
	if err != nil || r3.Pending || r3.Address != "10.90.4.1" || r3.PresharedKey == "" || r3.MachineSecret != "" {
		t.Fatalf("after approval: %+v %v", r3, err)
	}
	if hub, _ := svc.HubPeers(ctx); len(hub["peers"].([]HubPeer)) != 1 {
		t.Fatalf("approved machine not in the hub list")
	}
	// Blocking an approved machine does not make it pending again.
	_ = svc.SetBlocked(ctx, "NEWMAC0001", true)
	if v, _ := svc.View(ctx, nil, Material{}); v.Peers[0].Pending || v.Peers[0].State != "gesperrt" {
		t.Fatalf("blocked: %+v", v.Peers[0])
	}
}

func TestMachineSecret(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	first, _ := svc.Enroll(ctx, newScript(mac("SERIAL0001", key1, "a", 49152), ""))
	secret := first.MachineSecret
	_ = svc.SetBlocked(ctx, "SERIAL0001", false)
	ok, err := svc.Enroll(ctx, newScript(mac("SERIAL0001", key1, "a", 49152), secret))
	if err != nil || ok.PresharedKey == "" {
		t.Fatalf("with secret: %+v %v", ok, err)
	}
	// Without the secret, or with a wrong one, the serial cannot be taken over — not even with the right key.
	for _, req := range []EnrollRequest{mac("SERIAL0001", key1, "a", 49152), newScript(mac("SERIAL0001", key1, "a", 49152), ""), newScript(mac("SERIAL0001", key1, "a", 49152), secret+"x"), newScript(mac("SERIAL0001", key2, "evil", 49152), "")} {
		if r, err := svc.Enroll(ctx, req); status(err) != 409 || r.PresharedKey != "" || r.Address != "" {
			t.Fatalf("takeover %v: %+v %v", req, r, err)
		}
	}
	// The machine itself may change its key (it has the secret), and gets a NEW preshared key, never the old one for the new key.
	rekey, err := svc.Enroll(ctx, newScript(mac("SERIAL0001", key2, "a", 49152), secret))
	if err != nil || rekey.Address != ok.Address || rekey.PresharedKey == "" || rekey.PresharedKey == ok.PresharedKey {
		t.Fatalf("key change with secret: %+v %v", rekey, err)
	}
	// Admin reset: the machine enrols fresh, awaits approval and gets a new secret; its old one is meaningless now.
	if err := svc.Forget(ctx, "SERIAL0001"); err != nil {
		t.Fatal(err)
	}
	fresh, err := svc.Enroll(ctx, newScript(mac("SERIAL0001", key2, "a", 49152), secret))
	if err != nil || !fresh.Pending || fresh.MachineSecret == "" || fresh.MachineSecret == secret {
		t.Fatalf("after reset: %+v %v", fresh, err)
	}
}

// Machines enrolled before the secret, and scripts that cannot keep one: recognised by their key, upgraded when their script can.
func TestSecretMigration(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	now := time.Now()
	_ = st.Update(ctx, func([]Peer) ([]Peer, []string, error) {
		return []Peer{{Serial: "OLDMAC0001", Name: "old", Address: "10.90.4.7", PublicKey: key1, PresharedKey: "psk=", Transport: "wireguard", Controls: true, Enabled: true, LastSeen: &now}}, nil, nil
	})
	// An attacker with the shared token and their own key: refused, and no preshared key in the answer.
	if r, err := svc.Enroll(ctx, mac("OLDMAC0001", key2, "evil", 49152)); status(err) != 409 || r.PresharedKey != "" {
		t.Fatalf("own key on an existing serial: %+v %v", r, err)
	}
	if r, err := svc.Enroll(ctx, newScript(mac("OLDMAC0001", key2, "evil", 49152), "")); status(err) != 409 || r.PresharedKey != "" {
		t.Fatalf("own key, new script: %+v %v", r, err)
	}
	// The old script on the machine: same key, no secret field — works as before, and is given no secret it would drop.
	old, err := svc.Enroll(ctx, mac("OLDMAC0001", key1, "old", 49152))
	if err != nil || old.Address != "10.90.4.7" || old.PresharedKey != "psk=" || old.MachineSecret != "" {
		t.Fatalf("old script: %+v %v", old, err)
	}
	// The self-updated script: same key, empty secret field — trust on first use, the secret is issued once.
	up, err := svc.Enroll(ctx, newScript(mac("OLDMAC0001", key1, "old", 49152), ""))
	if err != nil || up.MachineSecret == "" || up.PresharedKey != "psk=" {
		t.Fatalf("upgrade: %+v %v", up, err)
	}
	// From now on the key alone is not enough.
	if _, err := svc.Enroll(ctx, mac("OLDMAC0001", key1, "old", 49152)); status(err) != 409 {
		t.Fatalf("key without secret after the upgrade: %v", err)
	}
	if _, err := svc.Enroll(ctx, newScript(mac("OLDMAC0001", key1, "old", 49152), up.MachineSecret)); err != nil {
		t.Fatalf("with the secret: %v", err)
	}
}

func TestTransportAndAPIBaseLocked(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	join(t, svc, mac("SERIAL0001", key1, "a", 49152))
	toDirect := EnrollRequest{"serial": "SERIAL0001", "transport": "direct", "apiBase": "https://gpu.example/v1", "apiKey": "k", "ramMb": 49152.0}
	if _, err := svc.Enroll(ctx, toDirect); status(err) != 409 {
		t.Fatalf("wireguard -> direct: %v", err)
	}
	direct := EnrollRequest{"serial": "LINUXBOX01", "transport": "direct", "apiBase": "https://gpu.example/v1", "apiKey": "k", "ramMb": 64000.0, "machineSecret": ""}
	first, _ := svc.Enroll(ctx, direct)
	_ = svc.SetBlocked(ctx, "LINUXBOX01", false)
	direct["machineSecret"] = first.MachineSecret
	if _, err := svc.Enroll(ctx, direct); err != nil {
		t.Fatal(err)
	}
	// Even with its own secret a direct site cannot move its endpoint or become a tunnel peer.
	moved := EnrollRequest{"serial": "LINUXBOX01", "transport": "direct", "apiBase": "https://gpu.example/other", "apiKey": "k", "ramMb": 64000.0, "machineSecret": first.MachineSecret}
	if _, err := svc.Enroll(ctx, moved); status(err) != 409 {
		t.Fatalf("apiBase change: %v", err)
	}
	if _, err := svc.Enroll(ctx, newScript(mac("LINUXBOX01", key2, "x", 64000), first.MachineSecret)); status(err) != 409 {
		t.Fatalf("direct -> wireguard: %v", err)
	}
	sites, _ := svc.Sites(ctx)
	for _, s := range sites {
		if s.Transport == "direct" && s.APIBase != "https://gpu.example/v1" {
			t.Fatalf("site moved: %+v", s)
		}
	}
}

func TestAPIBaseValidation(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	bad := []string{
		"http://gpu.example/v1",             // no TLS
		"https://10.43.0.1:443",             // cluster service network
		"https://10.42.0.5/v1",              // cluster pod network
		"https://192.168.1.10/v1",           // RFC 1918
		"https://127.0.0.1:8000/v1",         // loopback
		"https://[::1]/v1",                  // loopback v6
		"https://[fd00::1]/v1",              // ULA
		"https://[fe80::1%25en0]/v1",        // link-local v6
		"https://169.254.169.254/latest",    // link-local, cloud metadata
		"https://100.64.0.1/v1",             // CGNAT
		"https://0.0.0.0/v1",                // unspecified
		"https://[::ffff:10.0.0.1]/v1",      // mapped private
		"https://inner.example/v1",          // one of its addresses is private
		"https://nowhere.example/v1",        // does not resolve
		"https://user:pw@gpu.example/v1",    // credentials in the URL
		"https://localhost/v1",              // resolves to nothing here, and loopback anywhere
		"ftp://gpu.example/v1",              // not https
		"https://gpu.example/v1?x=http://a", // query
	}
	for i, base := range bad {
		req := EnrollRequest{"serial": fmt.Sprintf("DIRECT%04d", i), "transport": "direct", "apiBase": base, "apiKey": "k", "ramMb": 64000.0, "contact": "it@dhbw.de"}
		if _, err := svc.Enroll(ctx, req); status(err) != 400 {
			t.Errorf("%s accepted: %v", base, err)
		}
	}
	for i, base := range []string{"https://gpu.example/v1", "https://141.72.1.12:8443/v1", "https://[2001:7c7:2000::12]/v1"} {
		req := EnrollRequest{"serial": fmt.Sprintf("GOOD%04d", i), "transport": "direct", "apiBase": base, "apiKey": "k", "ramMb": 64000.0}
		if _, err := svc.Enroll(ctx, req); err != nil {
			t.Errorf("%s refused: %v", base, err)
		}
	}
	if peers, _ := st.List(ctx); len(peers) != 3 {
		t.Fatalf("refused sites were recorded: %d", len(peers))
	}

	// The site list checks again: an entry from before the check, and a name that now resolves into the cluster, are left out.
	now := time.Now()
	_ = st.Update(ctx, func([]Peer) ([]Peer, []string, error) {
		return []Peer{
			{Serial: "LEGACY0001", Name: "legacy-internal", Transport: "direct", APIBase: "https://10.43.0.1:443", APIKey: "k", Enabled: true, LastSeen: &now},
			{Serial: "LEGACY0002", Name: "legacy-http", Transport: "direct", APIBase: "http://gpu.example/v1", APIKey: "k", Enabled: true, LastSeen: &now},
			{Serial: "LEGACY0003", Name: "rebound", Transport: "direct", APIBase: "https://rebind.example/v1", APIKey: "k", Enabled: true, LastSeen: &now},
			{Serial: "LEGACY0004", Name: "dns-down", Transport: "direct", APIBase: "https://down.example/v1", APIKey: "k", Enabled: true, LastSeen: &now},
		}, nil, nil
	})
	dns["rebind.example"] = []string{"10.43.0.1"}
	defer delete(dns, "rebind.example")
	sites, err := svc.Sites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range sites {
		names[s.Name] = true
	}
	if names["legacy-internal"] || names["legacy-http"] || names["rebound"] {
		t.Fatalf("internal site listed: %v", names)
	}
	// A failed lookup does not drop a site: a DNS hiccup must not deregister it.
	if !names["dns-down"] {
		t.Fatalf("site dropped on a failed lookup: %v", names)
	}
}

func TestPendingCap(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	for i := 0; i < maxPending; i++ {
		if _, err := svc.Enroll(ctx, EnrollRequest{"serial": fmt.Sprintf("SPAM%06d", i), "transport": "direct", "apiBase": "https://gpu.example/v1", "apiKey": "k", "ramMb": 64000.0}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Enroll(ctx, mac("ONEMORE001", key1, "x", 49152)); status(err) != 429 {
		t.Fatalf("pending cap: %v", err)
	}
}
