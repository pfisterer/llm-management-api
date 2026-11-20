package fleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// approved enrols a new machine with a script that keeps its secret, approves it and returns the secret.
func approved(t *testing.T, svc *Service, serial, key string) string {
	t.Helper()
	ctx := context.Background()
	r, err := svc.Enroll(ctx, newScript(mac(serial, key, strings.ToLower(serial), 49152), ""))
	if err != nil || r.MachineSecret == "" {
		t.Fatalf("enrol %s: %+v %v", serial, r, err)
	}
	if err := svc.SetBlocked(ctx, serial, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enroll(ctx, newScript(mac(serial, key, strings.ToLower(serial), 49152), r.MachineSecret)); err != nil {
		t.Fatal(err)
	}
	return r.MachineSecret
}

func checkReq(serial, secret string) EnrollRequest {
	return EnrollRequest{"serial": serial, "machineSecret": secret}
}

func TestCheckAnswersLikeEnrolment(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	secret := approved(t, svc, "SERIAL0001", key1)
	r, err := svc.Check(ctx, checkReq("SERIAL0001", secret))
	if err != nil {
		t.Fatal(err)
	}
	full, _ := svc.Enroll(ctx, newScript(mac("SERIAL0001", key1, "serial0001", 49152), secret))
	if r.ScriptManifest == "" || r.ScriptManifest != full.ScriptManifest || strings.Count(r.ScriptManifest, ":") != 3 {
		t.Fatalf("check %q, enrolment %q", r.ScriptManifest, full.ScriptManifest)
	}
	// Self-update off: nothing to compare, like the enrolment answer.
	svc.cfg.SelfUpdate = false
	if r, err := svc.Check(ctx, checkReq("SERIAL0001", secret)); err != nil || r.ScriptManifest != "" {
		t.Fatalf("selfUpdate off: %+v %v", r, err)
	}
}

func TestCheckCanary(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, func(c *Config) { c.SelfUpdateCanary = []string{"CANARY0001"} })
	canary := approved(t, svc, "CANARY0001", key1)
	other := approved(t, svc, "OTHER00001", key2)
	if r, err := svc.Check(ctx, checkReq("CANARY0001", canary)); err != nil || strings.Count(r.ScriptManifest, ":") != 3 {
		t.Fatalf("canary: %+v %v", r, err)
	}
	if r, err := svc.Check(ctx, checkReq("OTHER00001", other)); err != nil || r.ScriptManifest != "" {
		t.Fatalf("non-canary must get no manifest: %+v %v", r, err)
	}
}

func TestCheckRefuses(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := testService(t, nil)
	secret := approved(t, svc, "SERIAL0001", key1)
	// Pending: enrolled, not approved.
	pending, _ := svc.Enroll(ctx, newScript(mac("PENDING001", key2, "neu", 49152), ""))
	// Migration case: enrolled by an old script, recognised by its key only, no secret stored.
	join(t, svc, mac("OLDMAC0001", key0, "alt", 49152))
	for _, c := range []struct {
		name string
		req  EnrollRequest
		want int
	}{
		{"invalid serial", checkReq("x", secret), 400},
		{"unknown", checkReq("UNKNOWN001", secret), 403},
		{"no secret", checkReq("SERIAL0001", ""), 403},
		{"wrong secret", checkReq("SERIAL0001", secret+"x"), 403},
		{"secret of another machine", checkReq("PENDING001", secret), 403},
		{"pending", checkReq("PENDING001", pending.MachineSecret), 403},
		{"no stored secret", checkReq("OLDMAC0001", ""), 403},
	} {
		if _, err := svc.Check(ctx, c.req); status(err) != c.want {
			t.Errorf("%s: want %d, got %v", c.name, c.want, err)
		}
	}
	// Blocked after approval.
	_ = svc.SetBlocked(ctx, "SERIAL0001", true)
	if _, err := svc.Check(ctx, checkReq("SERIAL0001", secret)); status(err) != 403 {
		t.Fatalf("blocked: %v", err)
	}
	_ = svc.SetBlocked(ctx, "SERIAL0001", false)
	// Not in allowedSerials.
	svc.cfg.AllowedSerials = []string{"OTHER00001"}
	if _, err := svc.Check(ctx, checkReq("SERIAL0001", secret)); status(err) != 403 {
		t.Fatalf("not allowed: %v", err)
	}
	svc.cfg.AllowedSerials = nil
	svc.cfg.Enabled = false
	if _, err := svc.Check(ctx, checkReq("SERIAL0001", secret)); status(err) != 503 {
		t.Fatalf("disabled: %v", err)
	}
}

// A check neither registers a machine nor touches the record of a known one (not even last_seen).
func TestCheckIsReadOnly(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := testService(t, nil)
	secret := approved(t, svc, "SERIAL0001", key1)
	before, _ := st.List(ctx)
	svc.now = func() time.Time { return before[0].LastSeen.Add(time.Hour) }
	if _, err := svc.Check(ctx, checkReq("SERIAL0001", secret)); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Check(ctx, checkReq("UNKNOWN001", secret))
	after, _ := st.List(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("check changed the store:\n%+v\n%+v", before, after)
	}
}
