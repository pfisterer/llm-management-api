package gpu

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// ensureUser asks for the person and creates them only if the hub does not know them, with a POST without any body: the hub's token has no admin:users, its custom scope creates users only from an empty request (aiaas-deployment, 05-dhbw-access).
func TestHubEnsureUser(t *testing.T) {
	var mu sync.Mutex
	known := map[string]bool{"old@x": true}
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		name := r.URL.Path[len("/hub/api/users/"):]
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if !known[name] {
				http.Error(w, `{"status":404,"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			w.Write([]byte(`{"name":"` + name + `","servers":{}}`))
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if len(body) > 0 {
				http.Error(w, `{"status":400,"message":"This token creates users only with an empty request"}`, http.StatusBadRequest)
				return
			}
			known[name] = true
			created = append(created, name)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"name":"` + name + `","admin":false}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	h := newHub(srv.URL, "tok")
	ctx := context.Background()
	for _, name := range []string{"old@x", "new@x", "new@x"} {
		if err := h.ensureUser(ctx, name); err != nil {
			t.Fatalf("ensureUser(%s): %v", name, err)
		}
	}
	if len(created) != 1 || created[0] != "new@x" {
		t.Fatalf("created %v, want [new@x]", created)
	}
}
