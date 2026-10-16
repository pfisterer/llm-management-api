package gpu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The hub's progress stream as the start page reads it: events so far, the end (ready or failed with the reason), a server that is not starting, and a start that goes on beyond the window.
func TestHubProgress(t *testing.T) {
	progressWindow = 300 * time.Millisecond
	t.Cleanup(func() { progressWindow = 2 * time.Second })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); w.(http.Flusher).Flush() }
		switch r.URL.Path {
		case "/hub/api/users/a@x/servers/ok/progress":
			send(`{"progress":0,"message":"Server requested"}`)
			send(`{"progress":50,"message":"Pulling image"}`)
			send(`{"progress":100,"ready":true,"message":"Server ready","url":"/user/a@x/ok/"}`)
		case "/hub/api/users/a@x/servers/bad/progress":
			send(`{"progress":0,"message":"Server requested"}`)
			send(`{"progress":100,"failed":true,"message":"Spawn failed: Timeout"}`)
		case "/hub/api/users/a@x/servers/idle/progress":
			http.Error(w, `{"status":400,"message":"idle is not starting"}`, http.StatusBadRequest)
		case "/hub/api/users/a@x/servers/slow/progress":
			send(`{"progress":10,"message":"Waiting for a GPU"}`)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := newHub(srv.URL, "tok")
	ctx := context.Background()

	p, err := h.progress(ctx, "a@x", "ok")
	if err != nil || !p.Ready || p.Failed || p.Starting || len(p.Events) != 3 || p.Events[1].Message != "Pulling image" {
		t.Fatalf("ok: %+v %v", p, err)
	}
	p, err = h.progress(ctx, "a@x", "bad")
	if err != nil || !p.Failed || p.Message != "Spawn failed: Timeout" || len(p.Events) != 1 {
		t.Fatalf("bad: %+v %v", p, err)
	}
	p, err = h.progress(ctx, "a@x", "idle")
	if err != nil || p.Starting || p.Failed || p.Ready || len(p.Events) != 0 {
		t.Fatalf("idle: %+v %v", p, err)
	}
	start := time.Now()
	p, err = h.progress(ctx, "a@x", "slow")
	if err != nil || !p.Starting || len(p.Events) != 1 || p.Events[0].Progress != 10 {
		t.Fatalf("slow: %+v %v", p, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("slow took %s, want about the window", d)
	}
	if _, err := h.progress(ctx, "a@x", "missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}

// What an init container of the server reports (its termination message, e.g. the course material could not be fetched) is a line of the start's progress; another person's pod of the same server name adds nothing.
func TestServerProgressInitMessages(t *testing.T) {
	s, _, _, _ := testService()
	s.pods = fakePods{items: `[
		{"metadata":{"name":"p1","annotations":{"hub.jupyter.org/username":"a@x"}},"status":{"initContainerStatuses":[
			{"name":"dhbw-dirs","state":{"terminated":{"message":""}}},
			{"name":"git-checkout","state":{"terminated":{"message":"Material aus https://github.com/o/kurs konnte nicht geholt werden.\n"}}}]}},
		{"metadata":{"name":"p2","annotations":{"hub.jupyter.org/username":"b@x"}},"status":{"initContainerStatuses":[
			{"name":"git-checkout","state":{"terminated":{"message":"fremd"}}}]}}]`}
	p, err := s.ServerProgress(context.Background(), "a@x", "srv")
	if err != nil || len(p.Events) != 1 || p.Events[0].Message != "Material aus https://github.com/o/kurs konnte nicht geholt werden." {
		t.Fatalf("progress %+v %v", p, err)
	}
}
