package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/litellm"
)

// fakeLiteLLM mimics the parts of the LiteLLM management API used here,
// including its global uniqueness of key aliases.
type fakeLiteLLM struct {
	mu     sync.Mutex
	users  map[string]map[string]any
	keys   map[string]map[string]any // token -> key
	writes []string                  // "user/new:<id>", "user/update:<id>:quota" …
	n      int
}

func newFake() *fakeLiteLLM {
	return &fakeLiteLLM{users: map[string]map[string]any{}, keys: map[string]map[string]any{}}
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
		for _, k := range f.keys {
			if k["key"] == r.URL.Query().Get("key") {
				_ = json.NewEncoder(w).Encode(map[string]any{"info": k})
				return
			}
		}
		w.WriteHeader(404)
	case "/key/delete":
		for _, t := range body["keys"].([]any) {
			delete(f.keys, t.(string))
		}
	default:
		w.WriteHeader(404)
	}
}

var student = Tier{Name: "student", KeyBudget: 0.5, KeyBudgetDuration: "4h", UserBudget: 8, UserBudgetDuration: "30d", RPM: 60, TPM: 1000, Models: []string{"chat-default"}}
var staff = Tier{Name: "staff", KeyBudget: 2, KeyBudgetDuration: "4h", UserBudget: 40, UserBudgetDuration: "30d", RPM: 120, TPM: 4000, Models: []string{"code"}}

func setup(t *testing.T) (*Service, *fakeLiteLLM) {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewService(litellm.New(srv.URL, "master", 5*time.Second), 2), f
}

func TestUsageCreatesUserAndKeepsAdjustments(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}

	u, err := svc.Usage(ctx, p, student)
	if err != nil || u.Budget == nil || *u.Budget != 8 {
		t.Fatalf("first usage: %+v %v", u, err)
	}
	if f.writes[0] != "user/new:kc-abc" {
		t.Fatalf("expected user creation, got %v", f.writes)
	}
	// An admin raises the budget in the LiteLLM UI; the same tier must not revert it.
	f.users["kc-abc"]["max_budget"] = 99.0
	u, _ = svc.Usage(ctx, p, student)
	if *u.Budget != 99 {
		t.Fatalf("adjustment reverted: %v", *u.Budget)
	}
	// A tier change does write the new quota.
	u, _ = svc.Usage(ctx, p, staff)
	if *u.Budget != 40 || !strings.HasSuffix(f.writes[len(f.writes)-1], ":quota") {
		t.Fatalf("tier change not applied: %v %v", *u.Budget, f.writes)
	}
}

func TestCreateKeys(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	a := Person{Subject: "a", Email: "a@dhbw.de"}
	b := Person{Subject: "b", Email: "b@dhbw.de"}

	if _, _, err := svc.Create(ctx, a, student, "  "); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name: %v", err)
	}
	secret, v, err := svc.Create(ctx, a, student, "vs code")
	if err != nil || !strings.HasPrefix(secret, "sk-") || v.Name != "vs-code" {
		t.Fatalf("create: %q %+v %v", secret, v, err)
	}
	// The same name for ANOTHER person works: aliases carry the owner tag.
	if _, _, err := svc.Create(ctx, b, student, "vs code"); err != nil {
		t.Fatalf("same name, other person: %v", err)
	}
	// The same name again for the same person does not.
	if _, _, err := svc.Create(ctx, a, student, "vs-code"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	// A chat key exists but does not count (limit is 2 in this test).
	f.keys["chat"] = map[string]any{"token": "chat", "user_id": "kc-a", "key_alias": "librechat@" + aliasTag("kc-a")}
	if _, _, err := svc.Create(ctx, a, student, "laptop"); err != nil {
		t.Fatalf("second own key with chat key present: %v", err)
	}
	if _, _, err := svc.Create(ctx, a, student, "third"); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("limit: %v", err)
	}
	u, _ := svc.Usage(ctx, a, student)
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
	}
	// Key quota comes from the tier.
	for _, k := range f.keys {
		if k["user_id"] == "kc-a" && k["token"] != "chat" && k["max_budget"] != 0.5 {
			t.Fatalf("key quota: %v", k["max_budget"])
		}
	}
}

func TestDeleteOnlyOwnKeys(t *testing.T) {
	ctx := context.Background()
	svc, f := setup(t)
	a := Person{Subject: "a", Email: "a@dhbw.de"}
	b := Person{Subject: "b", Email: "b@dhbw.de"}
	_, _, _ = svc.Create(ctx, a, student, "x")
	_, _, _ = svc.Create(ctx, b, student, "y")
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
