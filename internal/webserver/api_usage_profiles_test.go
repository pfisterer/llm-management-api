package webserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/llm/usageprofiles"
	"go.uber.org/zap"
)

type fixedProfiles struct {
	rep *usageprofiles.Report
}

func (f fixedProfiles) Report() (usageprofiles.Report, bool) {
	if f.rep == nil {
		return usageprofiles.Report{}, false
	}
	return *f.rep, true
}

// Only the tree's token reads the profiles; before the first computation they are 503.
func TestUsageProfilesAPI(t *testing.T) {
	p := &fixedProfiles{}
	h := New(Options{DevMode: true, DevAuthUser: testDevUser, TreeToken: "tree-secret", Profiles: p, Log: zap.NewNop().Sugar()}).Router()
	for _, tok := range []string{"", "wrong"} {
		if code, _ := treeDo(h, "GET", "/v1/usage-profiles", tok, ""); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	if code, _ := treeDo(h, "GET", "/v1/usage-profiles", "tree-secret", ""); code != http.StatusServiceUnavailable {
		t.Errorf("before the first computation: %d", code)
	}
	p.rep = &usageprofiles.Report{From: time.Unix(0, 0), MinKeys: 5, Models: []usageprofiles.Profile{{Alias: "code", Keys: 6, InputTokensPerRequest: &usageprofiles.Stat{Median: 13000, P90: 20000, Samples: 60, Keys: 6}}}}
	h = New(Options{DevMode: true, DevAuthUser: testDevUser, TreeToken: "tree-secret", Profiles: *p, Log: zap.NewNop().Sugar()}).Router()
	code, out := treeDo(h, "GET", "/v1/usage-profiles", "tree-secret", "")
	if code != http.StatusOK || !strings.Contains(out, `"alias":"code"`) || !strings.Contains(out, `"median":13000`) {
		t.Errorf("profiles: %d %s", code, out)
	}
	// Without a tree token the route does not exist.
	h = New(Options{DevMode: true, DevAuthUser: testDevUser, Profiles: *p, Log: zap.NewNop().Sugar()}).Router()
	if code, _ := treeDo(h, "GET", "/v1/usage-profiles", "", ""); code != http.StatusNotFound && code != http.StatusUnauthorized {
		t.Errorf("without a tree token: %d", code)
	}
}
