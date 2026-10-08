package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pfisterer/llm-management-api/internal/access"
	"go.uber.org/zap"
)

// AdminLoginOptions configures the LiteLLM UI autologin.
type AdminLoginOptions struct {
	Access     *access.Service
	LiteLLMURL string // management backend, where /v2/login lives
	Username   string
	Password   string
	Log        *zap.SugaredLogger
}

// AdminLoginRouter signs admins in to the LiteLLM UI without a second form.
//
// LiteLLM knows only username/password or SSO (enterprise beyond five users)
// for its UI. The admin host sits behind the Keycloak forward-auth anyway, so
// the second login with the shared UI account was pure repetition. This handler
// logs in server-side with that account (POST /v2/login) and hands the session
// cookie "token" to the browser; the UI then loads signed in.
//
// Trust in X-Auth-Request-Email: this listener is reachable only through the
// admin ingress (NetworkPolicy: Traefik only), where the forward-auth
// middleware sets exactly this header from oauth2-proxy's answer for EVERY path
// and overwrites one sent by the client. The access rules are checked on top:
// only the role admin gets in.
//
// Actions in the UI still run under the shared account and cannot be traced to
// a person.
func AdminLoginRouter(o AdminLoginOptions) http.Handler {
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	login := func(w http.ResponseWriter, r *http.Request) {
		email := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Auth-Request-Email")))
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		d, _, err := o.Access.Decide(ctx, email)
		if email == "" || err != nil || !d.Role.AtLeast(access.RoleAdmin) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<!doctype html><meta charset=utf-8><title>DHBW LLMaaS</title>"+
				"<p style='font-family:system-ui;margin:15vh auto;max-width:30rem;text-align:center;color:#555'>"+
				"Kein Zugang zur LiteLLM-Verwaltung für "+html.EscapeString(orUnknown(email))+".</p>")
			return
		}
		body, _ := json.Marshal(map[string]string{"username": o.Username, "password": o.Password})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.LiteLLMURL, "/")+"/v2/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			o.Log.Errorw("autologin: LiteLLM unreachable", "error", err)
			http.Error(w, "LiteLLM nicht erreichbar", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		token := ""
		for _, c := range resp.Cookies() {
			if c.Name == "token" {
				token = c.Value
			}
		}
		if resp.StatusCode/100 != 2 && resp.StatusCode/100 != 3 || token == "" {
			o.Log.Errorw("autologin: LiteLLM login failed", "status", resp.StatusCode)
			http.Error(w, "Anmeldung bei LiteLLM fehlgeschlagen", http.StatusBadGateway)
			return
		}
		o.Log.Infow("autologin", "email", email)
		// No Domain: valid only for the admin host this answer comes from. Not
		// HttpOnly — the UI reads the token from JavaScript.
		http.SetCookie(w, &http.Cookie{Name: "token", Value: token, Path: "/", Secure: true, SameSite: http.SameSiteLaxMode})
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/ui/?login=success", http.StatusSeeOther)
	}
	mux.HandleFunc("GET /{$}", login)
	mux.HandleFunc("GET /autologin", login)
	return mux
}

func orUnknown(s string) string {
	if s == "" {
		return "unbekannt"
	}
	return s
}
