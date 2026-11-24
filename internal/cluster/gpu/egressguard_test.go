package gpu

import "testing"

func TestEgressGuardOffWithoutTarget(t *testing.T) {
	for _, target := range []string{"", "10.43.0.1", ":443"} {
		if g := egressGuardInit("img", target, nil); g != nil {
			t.Errorf("target %q: want no guard, got %v", target, g)
		}
	}
	if g := egressGuardInit("", "10.43.0.1:443", nil); g != nil {
		t.Errorf("no image: want no guard")
	}
}

func TestBuildJobStartsWithEgressGuard(t *testing.T) {
	job := buildJob(Config{EgressGuard: "10.43.0.1:443"}, "b", "builder", Environment{}, "ml", "amd64", "reg/envs/ml:1", "", false)
	spec := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits, _ := spec["initContainers"].([]any)
	if len(inits) != 1 {
		t.Fatalf("want one init container, got %d", len(inits))
	}
	g := inits[0].(map[string]any)
	if g["name"] != "dhbw-egress-guard" || g["image"] != "builder" {
		t.Fatalf("unexpected guard %v", g)
	}
	cmd := g["command"].([]any)
	if cmd[len(cmd)-2] != "10.43.0.1" || cmd[len(cmd)-1] != "443" {
		t.Fatalf("guard target not passed: %v", cmd[3:])
	}
	if _, ok := buildJob(Config{}, "b", "builder", Environment{}, "ml", "amd64", "reg/envs/ml:1", "", false)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["initContainers"]; ok {
		t.Fatal("without EgressGuard the build has no init container")
	}
}

func TestWithEgressGuardGoesFirst(t *testing.T) {
	spec := map[string]any{"initContainers": []any{map[string]any{"name": "dhbw-dirs"}}}
	withEgressGuard(spec, egressGuardInit("img", "10.43.0.1:443", nil))
	inits := spec["initContainers"].([]any)
	if len(inits) != 2 || inits[0].(map[string]any)["name"] != "dhbw-egress-guard" {
		t.Fatalf("guard not first: %v", inits)
	}
}
