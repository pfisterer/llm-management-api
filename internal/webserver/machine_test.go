package webserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/fleet"
	"go.uber.org/zap"
)

func TestMachineTokens(t *testing.T) {
	cfg, wg, err := fleet.ParseConfig(`{"enabled":true,"enrollToken":"enroll-tok","adminToken":"admin-tok","selfUpdate":true,
		"profiles":[{"name":"l","minRamMb":46000,"model":"big","context":32768,"wiredLimitMb":30720,"weight":3}]}`, `{"hubPublicKey":"HUB="}`)
	if err != nil {
		t.Fatal(err)
	}
	h := MachineRouter(fleet.NewService(cfg, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir())), nil, zap.NewNop().Sugar(), true)

	req := func(method, path, token, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	enrollBody := `{"serial":"XL45R9YKW7","publicKey":"MQPtG7EOhnJh3azU3T1lmwx9A92KygkRcwH9n2j1zCc=","ramMb":49152,"hostname":"wimac02"}`

	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"enroll without token", "POST", "/enroll", "", enrollBody, http.StatusUnauthorized},
		{"enroll with admin token", "POST", "/enroll", "admin-tok", enrollBody, http.StatusUnauthorized},
		// A new machine is recorded and awaits approval (202); the second ask is still pending.
		{"enroll new machine", "POST", "/enroll", "enroll-tok", enrollBody, http.StatusAccepted},
		{"enroll pending machine", "POST", "/enroll", "enroll-tok", enrollBody, http.StatusAccepted},
		{"invalid json", "POST", "/enroll", "enroll-tok", "{", http.StatusBadRequest},
		// The enrolment token must NOT read the peer list (it carries every PSK).
		{"peers with enroll token", "GET", "/fleet/peers", "enroll-tok", "", http.StatusUnauthorized},
		{"peers", "GET", "/fleet/peers", "admin-tok", "", http.StatusOK},
		{"sites", "GET", "/fleet/sites", "admin-tok", "", http.StatusOK},
		{"unknown script", "GET", "/scripts/nope.sh", "enroll-tok", "", http.StatusNotFound},
		// Person-facing routes do not exist on this listener at all.
		{"no person api here", "GET", "/v1/me", "admin-tok", "", http.StatusNotFound},
	}
	for _, c := range cases {
		if got := req(c.method, c.path, c.token, c.body); got != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, got)
		}
	}
}

// The answer for a machine awaiting approval: the secret once, and the word old scripts take as a decision rather than a failure.
func TestEnrollPendingAnswer(t *testing.T) {
	cfg, wg, err := fleet.ParseConfig(`{"enabled":true,"enrollToken":"enroll-tok","adminToken":"admin-tok",
		"profiles":[{"name":"l","minRamMb":46000,"model":"big","context":32768,"wiredLimitMb":30720,"weight":3}]}`, `{"hubPublicKey":"HUB="}`)
	if err != nil {
		t.Fatal(err)
	}
	svc := fleet.NewService(cfg, wg, fleet.NewMemoryStore(), fleet.NewScripts(t.TempDir()))
	h := MachineRouter(svc, nil, zap.NewNop().Sugar(), true)
	enroll := func(body string) (int, map[string]any) {
		r := httptest.NewRequest("POST", "/enroll", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer enroll-tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	body := `{"serial":"XL45R9YKW7","publicKey":"MQPtG7EOhnJh3azU3T1lmwx9A92KygkRcwH9n2j1zCc=","ramMb":49152,"hostname":"wimac02","machineSecret":""}`
	code, out := enroll(body)
	secret, _ := out["machineSecret"].(string)
	if code != http.StatusAccepted || out["status"] != "pending" || secret == "" || !strings.Contains(fmt.Sprint(out["message"]), "blocked") || out["presharedKey"] != nil {
		t.Fatalf("pending answer: %d %v", code, out)
	}
	if err := svc.SetBlocked(context.Background(), "XL45R9YKW7", false); err != nil {
		t.Fatal(err)
	}
	// Approved: the full answer with the secret; without it a 409.
	withSecret := strings.Replace(body, `"machineSecret":""`, `"machineSecret":"`+secret+`"`, 1)
	if code, out := enroll(withSecret); code != http.StatusOK || out["presharedKey"] == "" || out["address"] == "" {
		t.Fatalf("approved: %d %v", code, out)
	}
	if code, out := enroll(body); code != http.StatusConflict || out["presharedKey"] != nil {
		t.Fatalf("without the secret: %d %v", code, out)
	}
}
