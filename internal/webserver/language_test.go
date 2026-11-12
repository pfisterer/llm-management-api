package webserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
)

// languages are the Accept-Language headers a test sends, and which of the two texts each must get: German without one and for anything that names neither language, English when the caller asks for it.
var languages = []struct {
	header  string
	english bool
}{
	{"", false},
	{"de-DE,de;q=0.9", false},
	{"fr", false},
	{"en", true},
	{"en-US,en;q=0.9,de;q=0.8", true},
}

// callLang is call with an Accept-Language header (none when empty) and the whole response.
func callLang(t *testing.T, h http.Handler, method, path, bearer, lang string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return body
}

// The same REST request answers in German without Accept-Language (and for a language that is neither) and in English with Accept-Language: en; the status and the code do not change.
func TestREST_AnswersInTheCallersLanguage(t *testing.T) {
	e := newJobEnv(t)
	member, nobody := "oidc:"+jzMember, "oidc:"+jzNobody
	for _, tc := range []struct {
		name, method, path, bearer string
		body                       any
		status                     int
		code, de, en               string
	}{
		{"no credential", http.MethodGet, "/v1/me", "", nil, http.StatusUnauthorized, "unauthenticated", "Anmeldung erforderlich.", "Sign-in required."},
		{"a project's refusal of a job", http.MethodPost, "/v1/gpu/jobs", member, SubmitJobRequest{Project: "t-fremd", Command: "true"}, http.StatusConflict, "refused", "Projekt nicht gefunden oder kein Zugang.", "Project not found or no access."},
		{"a job's validation", http.MethodPost, "/v1/gpu/jobs", member, SubmitJobRequest{Project: "t-kurs"}, http.StatusBadRequest, "invalid", "Entweder einen Befehl oder ein Notebook angeben.", "Give either a command or a notebook."},
		{"the shared error table", http.MethodGet, "/v1/gpu/jobs/j-0123456789", member, nil, http.StatusNotFound, "not_found", "Job nicht gefunden.", "Job not found."},
		{"paging of golib", http.MethodGet, "/v1/gpu/jobs?limit=0", member, nil, http.StatusBadRequest, "invalid", "Ungültige Angaben zum Blättern (limit, offset): limit muss größer als 0 sein.", "Invalid paging (limit, offset): limit must be greater than 0."},
		{"no workspaces", http.MethodGet, "/v1/gpu/servers", nobody, nil, http.StatusForbidden, "forbidden", "Kein Zugang zu Workspaces (kein Projekt).", "No access to workspaces (no project)."},
		{"no LLM tokens", http.MethodGet, "/v1/keys", nobody, nil, http.StatusForbidden, "no_quota", "Kein Projekt mit LLM-Tokens.", "No project with LLM tokens."},
		{"a token's refusal", http.MethodPost, "/v1/tokens", e.token(t, jzMember, false), CreateAPITokenRequest{}, http.StatusForbidden, "token_via_token", "API-Tokens lassen sich nur nach einer Anmeldung erstellen, nicht mit einem anderen API-Token.", "API tokens can only be created after signing in, not with another API token."},
	} {
		for _, l := range languages {
			rec := callLang(t, e.h, tc.method, tc.path, tc.bearer, l.header, tc.body)
			body := errorBody(t, rec)
			want := tc.de
			if l.english {
				want = tc.en
			}
			if rec.Code != tc.status || body.Code != tc.code || body.Error != want {
				t.Errorf("%s, Accept-Language %q: got %d %q %q, want %d %q %q", tc.name, l.header, rec.Code, body.Code, body.Error, tc.status, tc.code, want)
			}
			if vary := rec.Header().Get("Vary"); vary != "Accept-Language" {
				t.Errorf("%s: Vary = %q", tc.name, vary)
			}
		}
	}
}

// An MCP tool's refusal is in the language of the HTTP request that carried the call.
func TestMCP_AnswersInTheCallersLanguage(t *testing.T) {
	e := newJobEnv(t)
	endpoint := mcpEndpoint(t, e.h)
	member, nobody := e.token(t, jzMember, false), e.token(t, jzNobody, true)
	for _, l := range languages {
		for _, tc := range []struct {
			bearer, tool string
			args         map[string]any
			de, en       string
		}{
			{member, "submit_job", map[string]any{"project": "t-fremd", "command": "true"}, "Projekt nicht gefunden oder kein Zugang.", "Project not found or no access."},
			{member, "submit_job", map[string]any{"project": ""}, "project fehlt.", "project is missing."},
			{member, "submit_job", map[string]any{"project": "t-kurs"}, "Entweder einen Befehl oder ein Notebook angeben.", "Give either a command or a notebook."},
			{member, "get_job", map[string]any{"id": "j-0123456789"}, "Job nicht gefunden.", "Job not found."},
			{nobody, "list_my_servers", nil, "Kein Zugang zu Workspaces (kein Projekt).", "No access to workspaces (no project)."},
		} {
			session := mcptest.DialLang(t, endpoint, tc.bearer, l.header)
			res := mcptest.Call(t, session, tc.tool, tc.args)
			want := tc.de
			if l.english {
				want = tc.en
			}
			if got := mcptest.Text(res); !res.IsError || got != want {
				t.Errorf("%s, Accept-Language %q: got %q (error %v), want %q", tc.tool, l.header, got, res.IsError, want)
			}
		}
	}
}

// On the hub listener the workspace routes answer people in their language, while JupyterHub's own endpoints keep their German texts whatever the request says.
func TestHubListener_Languages(t *testing.T) {
	_, r := workspaceEnv(t)
	for _, l := range languages {
		rec := callLang(t, r, http.MethodGet, "/v1/gpu/jobs", "", l.header, nil)
		want := "Nur aus dem eigenen Workspace mit seinem Hub-Token (JUPYTERHUB_API_TOKEN)."
		if l.english {
			want = "Only from your own workspace with its hub token (JUPYTERHUB_API_TOKEN)."
		}
		if body := errorBody(t, rec); rec.Code != http.StatusUnauthorized || body.Code != "unauthenticated" || body.Error != want {
			t.Errorf("workspace route, Accept-Language %q: got %d %+v, want 401 %q", l.header, rec.Code, body, want)
		}
		rec = callLang(t, r, http.MethodPost, "/gpu/hub/spawn-check", "wrong", l.header, nil)
		if body := errorBody(t, rec); rec.Code != http.StatusUnauthorized || body.Error != "Token fehlt oder ist falsch." {
			t.Errorf("JupyterHub's endpoint, Accept-Language %q: got %d %+v, want 401 in German", l.header, rec.Code, body)
		}
	}
}
