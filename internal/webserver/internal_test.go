package webserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"go.uber.org/zap"
)

func TestInternalAccess(t *testing.T) {
	roles := &roleprovider.Mock{Memberships: map[string][]string{"student@dhbw.de": {"group:wwi23seb"}}}
	svc, err := access.NewService(access.NewMemoryStore(), roles, access.Config{
		Tiers: []string{"student", "staff"}, BootstrapAdmins: []string{"admin@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), access.Rule{Token: "group:wwi23seb", Role: access.RoleUser, Tier: "student"}, "admin@dhbw.de"); err != nil {
		t.Fatal(err)
	}
	h := InternalRouter(svc, zap.NewNop().Sugar(), true)

	get := func(q string) (int, AccessAnswer) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/v1/access"+q, nil))
		var a AccessAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &a)
		return rec.Code, a
	}

	cases := []struct {
		name, query string
		wantCode    int
		wantRole    access.Role
		wantTier    string
	}{
		{"group rule", "?email=Student@dhbw.de", http.StatusOK, access.RoleUser, "student"},
		{"bootstrap admin", "?email=admin@dhbw.de", http.StatusOK, access.RoleAdmin, "staff"},
		{"no rule", "?email=other@dhbw.de", http.StatusOK, access.RoleNone, ""},
		{"no email", "", http.StatusBadRequest, access.RoleNone, ""},
	}
	for _, tc := range cases {
		code, a := get(tc.query)
		if code != tc.wantCode || a.Role != tc.wantRole || a.Tier != tc.wantTier {
			t.Errorf("%s: got %d %+v, want %d %s/%s", tc.name, code, a, tc.wantCode, tc.wantRole, tc.wantTier)
		}
	}
}
