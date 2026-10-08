package webserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"go.uber.org/zap"
)

func TestAdminLogin(t *testing.T) {
	lite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/login" {
			w.WriteHeader(404)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "token", Value: "jwt-123"})
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
	}))
	defer lite.Close()
	svc, err := access.NewService(access.NewMemoryStore(), &roleprovider.Mock{}, access.Config{
		Tiers: []string{"staff"}, BootstrapAdmins: []string{"admin@dhbw.de"}, BootstrapAdminTier: "staff",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := AdminLoginRouter(AdminLoginOptions{Access: svc, LiteLLMURL: lite.URL, Username: "u", Password: "p", Log: zap.NewNop().Sugar()})
	get := func(path, email string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if email != "" {
			r.Header.Set("X-Auth-Request-Email", email)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec := get("/autologin", "Admin@dhbw.de")
	if rec.Code != 303 || rec.Header().Get("Location") != "/ui/?login=success" {
		t.Fatalf("admin: %d %v", rec.Code, rec.Header())
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Value != "jwt-123" {
		t.Fatalf("cookie: %v", c)
	}
	for _, email := range []string{"", "other@dhbw.de"} {
		if rec := get("/", email); rec.Code != 403 {
			t.Errorf("%q: %d", email, rec.Code)
		}
	}
	if rec := get("/ui/", "admin@dhbw.de"); rec.Code != 404 {
		t.Errorf("other path: %d", rec.Code)
	}
}
