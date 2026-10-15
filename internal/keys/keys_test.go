package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/litellm"
)

// fakeLiteLLM mimics the parts of the LiteLLM management API used here,
// including its global uniqueness of key aliases and its rule that a key moves
// into a team only when its user is a member there.
type fakeLiteLLM struct {
	mu     sync.Mutex
	users  map[string]map[string]any
	keys   map[string]map[string]any // token -> key
	teams  map[string]map[string]any // team id -> team
	writes []string                  // "user/new:<id>", "user/update:<id>:quota", "team/new:<id>", "key/update:<token>:<team>" …
	n      int
}

func newFake() *fakeLiteLLM {
	return &fakeLiteLLM{users: map[string]map[string]any{}, keys: map[string]map[string]any{}, teams: map[string]map[string]any{}}
}

func (f *fakeLiteLLM) keyBy(ref string) map[string]any {
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

func isMember(team map[string]any, uid string) bool {
	ms, _ := team["members_with_roles"].([]any)
	for _, m := range ms {
		if m.(map[string]any)["user_id"] == uid {
			return true
		}
	}
	return false
}

func (f *fakeLiteLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/user/info":
		uid := r.URL.Query().Get("user_id")
		u, ok := f.users[uid]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var ks []map[string]any
		for _, k := range f.keys {
			if k["user_id"] == uid {
				ks = append(ks, k)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_info": u, "keys": ks})
	case "/user/new":
		uid := body["user_id"].(string)
		f.users[uid] = body
		f.writes = append(f.writes, "user/new:"+uid)
	case "/user/update":
		uid := body["user_id"].(string)
		maps.Copy(f.users[uid], body)
		tag := ""
		if _, ok := body["max_budget"]; ok {
			tag = ":quota"
		}
		f.writes = append(f.writes, "user/update:"+uid+tag)
	case "/key/generate":
		for _, k := range f.keys {
			if k["key_alias"] == body["key_alias"] {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"message":"Key with alias already exists"}}`)
				return
			}
		}
		f.n++
		tok := fmt.Sprintf("hash%d", f.n)
		body["token"] = tok
		f.keys[tok] = body
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "sk-secret-" + tok})
	case "/key/info":
		if k := f.keyBy(r.URL.Query().Get("key")); k != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"key": r.URL.Query().Get("key"), "info": k})
			return
		}
		w.WriteHeader(404)
	case "/key/update":
		k := f.keyBy(body["key"].(string))
		if k == nil {
			w.WriteHeader(404)
			return
		}
		team, _ := body["team_id"].(string)
		if team != "" && team != k["team_id"] && !isMember(f.teams[team], k["user_id"].(string)) {
			w.WriteHeader(403)
			fmt.Fprintf(w, `{"error":{"message":"User=%s is not a member of the team=%s"}}`, k["user_id"], team)
			return
		}
		// Like LiteLLM: only what the request carries changes.
		for _, field := range []string{"team_id", "max_budget", "budget_duration", "rpm_limit", "tpm_limit", "models"} {
			if v, ok := body[field]; ok {
				k[field] = v
			}
		}
		if _, ok := body["team_id"]; ok {
			f.writes = append(f.writes, fmt.Sprintf("key/update:%s:%s", k["token"], team))
		} else {
			f.writes = append(f.writes, fmt.Sprintf("key/quota:%s", k["token"]))
		}
	case "/key/delete":
		// Like LiteLLM: by the hashed token or by the key itself.
		for _, t := range body["keys"].([]any) {
			for tok, k := range f.keys {
				if tok == t || k["key"] == t {
					delete(f.keys, tok)
				}
			}
			f.writes = append(f.writes, "key/delete:"+t.(string))
		}
	case "/team/info":
		t, ok := f.teams[r.URL.Query().Get("team_id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"team_id": t["team_id"], "team_info": t, "keys": []any{}})
	case "/team/list":
		list := []map[string]any{}
		for _, t := range f.teams {
			list = append(list, t)
		}
		_ = json.NewEncoder(w).Encode(list)
	case "/team/new":
		id := body["team_id"].(string)
		if _, ok := f.teams[id]; ok {
			w.WriteHeader(400)
			return
		}
		body["spend"] = 0.0
		body["members_with_roles"] = []any{}
		f.teams[id] = body
		f.writes = append(f.writes, "team/new:"+id)
	case "/team/update":
		id := body["team_id"].(string)
		maps.Copy(f.teams[id], body)
		tag := ""
		if _, ok := body["budget_duration"]; ok {
			tag = ":duration"
		}
		f.writes = append(f.writes, "team/update:"+id+tag)
	case "/team/member_add":
		t := f.teams[body["team_id"].(string)]
		m := body["member"].(map[string]any)
		if isMember(t, m["user_id"].(string)) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"User already in team."}}`)
			return
		}
		t["members_with_roles"] = append(t["members_with_roles"].([]any), m)
		f.writes = append(f.writes, "team/member_add:"+body["team_id"].(string))
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeLiteLLM) team(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, _ := f.keys[id]["team_id"].(string)
	return t
}

// Pools as the webserver makes them: the project's limits with defaults filled in, base models plus its own.
var poolA = Pool{Slug: "t-a", Name: "Kurs A", MTokens: 100, RPM: 60, TPM: 200000, Models: []string{"chat-default", "chat-fast", "embedding"}}
var poolB = Pool{Slug: "t-b", Name: "Kurs B", MTokens: 20, RPM: 120, TPM: 400000, Models: []string{"chat-default", "chat-fast", "code", "embedding"}}

func grant(pools ...Pool) Grant { return Grant{Pools: pools} }

func setup(t *testing.T) (*Service, *fakeLiteLLM) {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewService(litellm.New(srv.URL, "master", 5*time.Second), 2, KeyBudget{Share: 0.05, Window: "4h"}), f
}

func TestUsageCreatesUserAndKeepsAdjustments(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}

	u, err := svc.Usage(ctx, p, grant(poolA, poolB))
	if err != nil || f.users["kc-abc"]["max_budget"] != 0.0 || f.users["kc-abc"]["budget_duration"] != "30d" {
		t.Fatalf("first usage: %+v %v %v", u, err, f.users["kc-abc"])
	}
	// A new user gets no limits and no models of their own: the teams' apply.
	for _, field := range []string{"rpm_limit", "tpm_limit", "models"} {
		if _, ok := f.users["kc-abc"][field]; ok {
			t.Fatalf("new user with %s: %v", field, f.users["kc-abc"])
		}
	}
	if f.writes[0] != "user/new:kc-abc" {
		t.Fatalf("expected user creation, got %v", f.writes)
	}
	// The pools: the first is the default; before the team exists, its budget is the project's and nothing is spent; the project's limits and the key budget are shown.
	if len(u.Projects) != 2 || !u.Projects[0].Default || u.Projects[1].Default || u.Projects[0].Budget != 10 || u.Projects[0].Spend != 0 || u.Projects[1].MTokens != 20 {
		t.Fatalf("projects: %+v", u.Projects)
	}
	if b := u.Projects[1]; b.RPM != 120 || b.TPM != 400000 || !slices.Equal(b.Models, poolB.Models) || math.Abs(b.KeyBudget-0.1) > 1e-9 || b.KeyBudgetDuration != "4h" {
		t.Fatalf("limits of B: %+v", b)
	}
	// Nothing differs: nothing is written.
	_, _ = svc.Usage(ctx, p, grant(poolA))
	if len(f.writes) != 1 {
		t.Fatalf("written without a difference: %v", f.writes)
	}
	// The budget goes back to 0 after a change in the LiteLLM UI; a user rate limit above what the projects allow together stays.
	f.users["kc-abc"]["max_budget"] = 99.0
	f.users["kc-abc"]["rpm_limit"] = 999
	_, _ = svc.Usage(ctx, p, grant(poolA))
	if f.users["kc-abc"]["max_budget"] != 0.0 || f.users["kc-abc"]["rpm_limit"] != 999 {
		t.Fatalf("quota not brought back: %v", f.users["kc-abc"])
	}
	// The team's numbers are shown once it exists.
	f.teams["t-a"] = map[string]any{"team_id": "t-a", "spend": 2.5, "max_budget": 10.0, "budget_duration": "30d", "budget_reset_at": "2026-11-01T00:00:00Z"}
	u, _ = svc.Usage(ctx, p, grant(poolA))
	if pu := u.Projects[0]; pu.Spend != 2.5 || pu.ResetAt == nil || *pu.ResetAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("team usage: %+v", pu)
	}
}

// A user rate limit (from the person-level tiers of before, or set in LiteLLM's UI) cannot be cleared through LiteLLM's API: it is raised to the sum of the person's pools' limits, so it never stops a key below its project's.
func TestUserLimitsRaised(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	f.users["kc-abc"] = map[string]any{"user_id": "kc-abc", "user_email": "a@dhbw.de", "max_budget": 0.0, "budget_duration": "30d", "rpm_limit": 60.0, "tpm_limit": 1000.0,
		"metadata": map[string]any{"tier": "student", "default_project": "t-b"}}
	info, _, _, _ := svc.lite.GetUser(ctx, "kc-abc")
	if wrote, err := svc.SyncQuota(ctx, info, grant(poolA, poolB)); !wrote || err != nil {
		t.Fatalf("not raised: %v %v", wrote, err)
	}
	u := f.users["kc-abc"]
	if u["rpm_limit"] != 180.0 || u["tpm_limit"] != 600000.0 || u["max_budget"] != 0.0 {
		t.Fatalf("after the sync: %v", u)
	}
	if meta := u["metadata"].(map[string]any); meta["tier"] != nil || meta["default_project"] != "t-b" || meta["source"] != "llm-management-api" {
		t.Fatalf("metadata: %v", meta)
	}
	info, _, _, _ = svc.lite.GetUser(ctx, "kc-abc")
	if wrote, err := svc.SyncQuota(ctx, info, grant(poolA, poolB)); wrote || err != nil {
		t.Fatalf("written again: %v %v", wrote, err)
	}
	// Fewer projects: a higher limit is not lowered.
	if wrote, _ := svc.SyncQuota(ctx, info, grant(poolA)); wrote {
		t.Fatal("lowered")
	}
}

func TestCreateKeys(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	a := Person{Subject: "a", Email: "a@dhbw.de"}
	b := Person{Subject: "b", Email: "b@dhbw.de"}
	ga := grant(poolA, poolB)

	if _, _, err := svc.Create(ctx, a, ga, "  ", ""); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name: %v", err)
	}
	secret, v, err := svc.Create(ctx, a, ga, "vs code", "")
	if err != nil || !strings.HasPrefix(secret, "sk-") || v.Name != "vs-code" || v.Project != "t-a" || v.ProjectName != "Kurs A" {
		t.Fatalf("create: %q %+v %v", secret, v, err)
	}
	// The pool's team was made with the project's budget, limits and models before the key went into it.
	if tm := f.teams["t-a"]; tm == nil || tm["max_budget"] != 10.0 || tm["budget_duration"] != "30d" || tm["team_alias"] != "Kurs A" || tm["metadata"].(map[string]any)["project"] != "t-a" ||
		tm["rpm_limit"] != 60.0 || tm["tpm_limit"] != 200000.0 || fmt.Sprint(tm["models"]) != "[chat-default chat-fast embedding]" {
		t.Fatalf("team: %v", f.teams["t-a"])
	}
	// The same name for ANOTHER person works: aliases carry the owner tag.
	if _, _, err := svc.Create(ctx, b, grant(poolA), "vs code", ""); err != nil {
		t.Fatalf("same name, other person: %v", err)
	}
	// The same name again for the same person does not.
	if _, _, err := svc.Create(ctx, a, ga, "vs-code", "t-b"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	// Only a project the person may draw from.
	if _, _, err := svc.Create(ctx, b, grant(poolA), "laptop", "t-b"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("project of others: %v", err)
	}
	// A chat key exists but does not count (limit is 2 in this test).
	f.keys["chat"] = map[string]any{"token": "chat", "user_id": "kc-a", "key_alias": "librechat@" + aliasTag("kc-a"), "team_id": "t-a"}
	_, v, err = svc.Create(ctx, a, ga, "laptop", "t-b")
	if err != nil || v.Project != "t-b" {
		t.Fatalf("second own key in the other project: %+v %v", v, err)
	}
	if _, _, err := svc.Create(ctx, a, ga, "third", ""); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("limit: %v", err)
	}
	u, _ := svc.Usage(ctx, a, ga)
	if u.OwnKeys != 2 || len(u.Keys) != 3 {
		t.Fatalf("usage keys: own=%d all=%d", u.OwnKeys, len(u.Keys))
	}
	for _, k := range u.Keys {
		if strings.Contains(k.Name, "@") {
			t.Fatalf("owner tag shown: %q", k.Name)
		}
		if k.ID == "chat" && (!k.Chat || k.Name != "DHBW-Chat (automatisch)") {
			t.Fatalf("chat key not marked: %+v", k)
		}
		if k.Name == "laptop" && (k.Project != "t-b" || k.ProjectName != "Kurs B") {
			t.Fatalf("key project: %+v", k)
		}
	}
	// A key carries its pool's short-term budget (5 % of it per 4h) and no limits or models of its own.
	for _, k := range f.keys {
		if k["user_id"] != "kc-a" || k["token"] == "chat" {
			continue
		}
		want := map[any]float64{"t-a": 0.5, "t-b": 0.1}[k["team_id"]]
		if mb, _ := k["max_budget"].(float64); math.Abs(mb-want) > 1e-9 || k["budget_duration"] != "4h" || k["rpm_limit"] != nil || k["tpm_limit"] != nil || k["models"] != nil {
			t.Fatalf("key quota: %v", k)
		}
	}
}

func TestDeleteOnlyOwnKeys(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	a := Person{Subject: "a", Email: "a@dhbw.de"}
	b := Person{Subject: "b", Email: "b@dhbw.de"}
	_, _, _ = svc.Create(ctx, a, grant(poolA), "x", "")
	_, _, _ = svc.Create(ctx, b, grant(poolA), "y", "")
	var bTok string
	for tok, k := range f.keys {
		if k["user_id"] == "kc-b" {
			bTok = tok
		}
	}
	if err := svc.Delete(ctx, a, bTok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting another person's key must look like not found, got %v", err)
	}
	if _, ok := f.keys[bTok]; !ok {
		t.Fatal("other person's key was deleted")
	}
	if err := svc.Delete(ctx, b, bTok); err != nil {
		t.Fatalf("own delete: %v", err)
	}
}

// SyncQuota writes only a quota that differs: the budget back to 0.
func TestSyncQuota(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}
	if _, err := svc.Usage(ctx, p, grant(poolA)); err != nil {
		t.Fatal(err)
	}
	u, _, _, err := svc.lite.GetUser(ctx, "kc-abc")
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := svc.SyncQuota(ctx, u, grant(poolA)); wrote || err != nil {
		t.Fatalf("same quota written: %v %v", wrote, err)
	}
	f.users["kc-abc"]["max_budget"] = 10.0
	u, _, _, _ = svc.lite.GetUser(ctx, "kc-abc")
	if wrote, err := svc.SyncQuota(ctx, u, grant(poolA)); !wrote || err != nil {
		t.Fatalf("budget not written: %v %v", wrote, err)
	}
	if f.users["kc-abc"]["max_budget"] != 0.0 || f.users["kc-abc"]["metadata"].(map[string]any)["source"] != "llm-management-api" {
		t.Fatalf("after sync: %v", f.users["kc-abc"])
	}
}

// Every pool has a team with the project's budget, rate limits and models; an ended project and a team whose project is gone keep their team with a budget of 0 (the gone one keeps its limits); teams of others are left alone; the window is sent only when it differs.
func TestSyncTeams(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	f.teams["t-gone"] = map[string]any{"team_id": "t-gone", "team_alias": "Alt", "max_budget": 5.0, "budget_duration": "30d", "spend": 3.0, "rpm_limit": 30.0, "models": []any{"chat-default"},
		"metadata": map[string]any{"source": "llm-management-api", "project": "t-gone"}}
	f.teams["foreign"] = map[string]any{"team_id": "foreign", "max_budget": 7.0, "metadata": map[string]any{}}
	f.teams["t-b"] = map[string]any{"team_id": "t-b", "team_alias": "Kurs B", "max_budget": 2.0, "budget_duration": "30d", "spend": 1.0, "rpm_limit": 120.0, "tpm_limit": 400000.0,
		"models": []any{"embedding", "code", "chat-fast", "chat-default"}, "metadata": map[string]any{"source": "llm-management-api", "project": "t-b"}}
	ended := poolB
	ended.MTokens = 0
	never := Pool{Slug: "t-never", Name: "Ohne Tokens", RPM: 60, TPM: 200000, Models: poolA.Models}

	n, err := svc.SyncTeams(ctx, []Pool{poolA, ended, never})
	if err != nil || n != 3 {
		t.Fatalf("sync: %d %v %v", n, err, f.writes)
	}
	if f.teams["t-a"]["max_budget"] != 10.0 || f.teams["t-b"]["max_budget"] != 0.0 || f.teams["t-gone"]["max_budget"] != 0.0 || f.teams["foreign"]["max_budget"] != 7.0 {
		t.Fatalf("budgets: a=%v b=%v gone=%v foreign=%v", f.teams["t-a"]["max_budget"], f.teams["t-b"]["max_budget"], f.teams["t-gone"]["max_budget"], f.teams["foreign"]["max_budget"])
	}
	if f.teams["t-gone"]["rpm_limit"] != 30.0 || fmt.Sprint(f.teams["t-gone"]["models"]) != "[chat-default]" {
		t.Fatalf("gone team's limits changed: %v", f.teams["t-gone"])
	}
	if _, ok := f.teams["t-never"]; ok {
		t.Fatal("team made for a project without tokens")
	}
	if f.teams["t-gone"]["spend"] != 3.0 || f.teams["t-gone"]["team_alias"] != "Alt" {
		t.Fatalf("gone team: %v", f.teams["t-gone"])
	}
	for _, w := range f.writes {
		if strings.HasSuffix(w, ":duration") {
			t.Fatalf("window restarted although it was the same: %v", f.writes)
		}
	}
	if n, err := svc.SyncTeams(ctx, []Pool{poolA, ended, never}); err != nil || n != 0 {
		t.Fatalf("second sync: %d %v", n, err)
	}
	// More tokens, a higher rate limit, one more model: the team follows.
	more := poolA
	more.MTokens, more.RPM, more.Models = 300, 90, []string{"chat-default", "chat-fast", "code", "embedding"}
	if n, _ := svc.SyncTeams(ctx, []Pool{more}); n != 1 || f.teams["t-a"]["max_budget"] != 30.0 || f.teams["t-a"]["rpm_limit"] != 90.0 || fmt.Sprint(f.teams["t-a"]["models"]) != "[chat-default chat-fast code embedding]" {
		t.Fatalf("raised: %d %v", n, f.teams["t-a"])
	}
	// No rate limit at all: sent as null.
	more.TPM = 0
	if n, _ := svc.SyncTeams(ctx, []Pool{more}); n != 1 || f.teams["t-a"]["tpm_limit"] != nil {
		t.Fatalf("no tpm: %d %v", n, f.teams["t-a"])
	}
	used, err := svc.UsedMTokens(ctx, []string{"t-b", "t-gone", "t-never"})
	if err != nil || used["t-b"] != 10 || used["t-gone"] != 30 || len(used) != 2 {
		t.Fatalf("used: %v %v", used, err)
	}
}

// The default project is stored in the user's metadata, only an eligible one, and the chat key follows it with that project's key budget.
func TestDefaultProject(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}
	g := grant(poolA, poolB)
	if _, err := svc.EnsureChatKey(ctx, p, g, "master"); err != nil {
		t.Fatal(err)
	}
	chat := f.keyBy(ChatKey("master", p.UserID()))
	if chat["team_id"] != "t-a" || chat["max_budget"] != 0.5 {
		t.Fatalf("chat key %v, want the first project with its key budget", chat)
	}
	if _, err := svc.SetDefaultProject(ctx, p, g, "t-other"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("other project: %v", err)
	}
	pool, err := svc.SetDefaultProject(ctx, p, g, "t-b")
	if err != nil || pool.Slug != "t-b" || f.users["kc-abc"]["metadata"].(map[string]any)["default_project"] != "t-b" {
		t.Fatalf("set: %+v %v %v", pool, err, f.users["kc-abc"])
	}
	if mb, _ := chat["max_budget"].(float64); chat["team_id"] != "t-b" || !isMember(f.teams["t-b"], "kc-abc") || math.Abs(mb-0.1) > 1e-9 {
		t.Fatalf("chat key not moved: %v %v", chat, f.teams["t-b"])
	}
	// The default stays when the quota is written again.
	f.users["kc-abc"]["max_budget"] = 5.0
	u, _ := svc.Usage(ctx, p, g)
	if !u.Projects[1].Default || u.Projects[0].Default || f.users["kc-abc"]["metadata"].(map[string]any)["default_project"] != "t-b" {
		t.Fatalf("default after the quota was written: %+v %v", u.Projects, f.users["kc-abc"]["metadata"])
	}
	// New keys go there when no project is named.
	if _, v, err := svc.Create(ctx, p, g, "laptop", ""); err != nil || v.Project != "t-b" {
		t.Fatalf("key without project: %+v %v", v, err)
	}
	// A default that is no longer eligible falls back to the first project.
	if _, v, err := svc.Create(ctx, p, grant(poolA), "tablet", ""); err != nil || v.Project != "t-a" {
		t.Fatalf("key after the default ended: %+v %v", v, err)
	}
}

// The sync moves keys outside the person's pools to the default project and takes keys out of every team when the person has no pool left; keys in an eligible pool stay. Every key gets its pool's short-term budget (0 outside every pool), and limits or models of its own are cleared.
func TestSyncKeys(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}
	_, _, _ = svc.Create(ctx, p, grant(poolA, poolB), "x", "t-a")
	_, _, _ = svc.Create(ctx, p, grant(poolA, poolB), "y", "t-b")
	// From the person-level tiers of before: no team, limits and models of its own.
	f.keys["old"] = map[string]any{"token": "old", "user_id": "kc-abc", "key_alias": "old@" + aliasTag("kc-abc"), "max_budget": 0.5, "budget_duration": "4h", "rpm_limit": 60.0, "tpm_limit": 1000.0, "models": []any{"chat-default"}}
	info, _, _, _ := svc.lite.GetUser(ctx, "kc-abc")

	// Only B is left: A's key and the old one move to B and get B's key budget; the old one loses its limits and models.
	n, err := svc.SyncKeys(ctx, info, grant(poolB))
	if err != nil || n != 4 {
		t.Fatalf("move: %d %v %v", n, err, f.writes)
	}
	for tok, k := range f.keys {
		if mb, _ := k["max_budget"].(float64); k["team_id"] != "t-b" || math.Abs(mb-0.1) > 1e-9 {
			t.Fatalf("key %s: %v, want in t-b with 0.1", tok, k)
		}
	}
	if old := f.keys["old"]; old["rpm_limit"] != nil || old["tpm_limit"] != nil || fmt.Sprint(old["models"]) != "[]" || old["budget_duration"] != "4h" {
		t.Fatalf("old key not cleared: %v", old)
	}
	if n, _ := svc.SyncKeys(ctx, info, grant(poolB)); n != 0 {
		t.Fatalf("moved again: %d", n)
	}
	// No pool left: every key leaves its team, so the user's budget of 0 stops it, and its own budget becomes 0.
	if n, err := svc.SyncKeys(ctx, info, grant()); err != nil || n != 6 {
		t.Fatalf("detach: %d %v", n, err)
	}
	for tok, k := range f.keys {
		if k["team_id"] != nil || k["max_budget"] != 0.0 {
			t.Fatalf("key %s: %v", tok, k)
		}
	}
	if n, _ := svc.SyncKeys(ctx, info, grant()); n != 0 {
		t.Fatalf("detached again: %d", n)
	}
	// A project again: the keys come back into its pool, with its key budget.
	if n, _ := svc.SyncKeys(ctx, info, grant(poolA)); n != 6 || f.team("old") != "t-a" || f.keys["old"]["max_budget"] != 0.5 {
		t.Fatalf("back: %d %v", n, f.keys["old"])
	}
}
