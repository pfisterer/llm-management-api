package fleet

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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
