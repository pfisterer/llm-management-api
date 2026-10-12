package webserver

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeLite is a LiteLLM management API with users, keys and teams — just enough for the key API, the chat exchange and the budget sync. Like LiteLLM it moves a key into a team only when the key's user is a member there.
type fakeLite struct {
	mu    sync.Mutex
	users map[string]map[string]any
	keys  map[string]map[string]any // token -> key; "key" holds a secret chosen by the caller
	teams map[string]map[string]any
	n     int
}

func newFakeLite(t *testing.T) (*fakeLite, *httptest.Server) {
	f := &fakeLite{users: map[string]map[string]any{}, keys: map[string]map[string]any{}, teams: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeLite) keyBy(ref string) map[string]any {
	if k, ok := f.keys[ref]; ok {
		return k
	}
	for _, k := range f.keys {
		if k["key"] == ref {
			return k
		}
	}
	return nil
}

func (f *fakeLite) member(team, uid string) bool {
	ms, _ := f.teams[team]["members_with_roles"].([]any)
	for _, m := range ms {
		if m.(map[string]any)["user_id"] == uid {
			return true
		}
	}
	return false
}

func (f *fakeLite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	q := r.URL.Query()
	switch r.URL.Path {
	case "/user/info":
		u, ok := f.users[q.Get("user_id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		ks := []map[string]any{}
		for _, k := range f.keys {
			if k["user_id"] == q.Get("user_id") {
				ks = append(ks, k)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_info": u, "keys": ks})
	case "/user/list":
		list := []map[string]any{}
		for _, u := range f.users {
			list = append(list, u)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": list, "total_pages": 1})
	case "/user/new":
		f.users[body["user_id"].(string)] = body
	case "/user/update":
		maps.Copy(f.users[body["user_id"].(string)], body)
	case "/key/generate":
		f.n++
		tok := fmt.Sprintf("hash%d", f.n)
		body["token"] = tok
		f.keys[tok] = body
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "sk-secret-" + tok})
	case "/key/info":
		if k := f.keyBy(q.Get("key")); k != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"info": k})
			return
		}
		w.WriteHeader(404)
	case "/key/update":
		k := f.keyBy(body["key"].(string))
		if k == nil {
			w.WriteHeader(404)
			return
		}
		if team, _ := body["team_id"].(string); team != "" && team != k["team_id"] && !f.member(team, k["user_id"].(string)) {
			w.WriteHeader(403)
			return
		}
		// Like LiteLLM: only what the request carries changes.
		for _, field := range []string{"team_id", "max_budget", "budget_duration", "rpm_limit", "tpm_limit", "models"} {
			if v, ok := body[field]; ok {
				k[field] = v
			}
		}
	case "/team/info":
		t, ok := f.teams[q.Get("team_id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"team_id": t["team_id"], "team_info": t})
	case "/team/list":
		list := []map[string]any{}
		for _, t := range f.teams {
			list = append(list, t)
		}
		_ = json.NewEncoder(w).Encode(list)
	case "/team/new":
		body["spend"] = 0.0
		body["members_with_roles"] = []any{}
		f.teams[body["team_id"].(string)] = body
	case "/team/update":
		maps.Copy(f.teams[body["team_id"].(string)], body)
	case "/team/member_add":
		t := f.teams[body["team_id"].(string)]
		t["members_with_roles"] = append(t["members_with_roles"].([]any), body["member"])
	default:
		w.WriteHeader(404)
	}
}

// fakeBackend is a fresh fakeLite's server.
func fakeBackend(t *testing.T) *httptest.Server {
	_, srv := newFakeLite(t)
	return srv
}
