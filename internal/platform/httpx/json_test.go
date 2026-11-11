package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer k" {
				http.Error(w, "headers", http.StatusBadRequest)
				return
			}
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			_ = json.NewEncoder(w).Encode(in)
		case "/patch":
			_, _ = w.Write([]byte(r.Header.Get("Content-Type")))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/long":
			http.Error(w, strings.Repeat("x", 5000), http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, c := context.Background(), srv.Client()

	var out map[string]any
	if status, err := Do(ctx, c, Request{Method: http.MethodPost, URL: srv.URL + "/echo", Header: http.Header{"Authorization": {"Bearer k"}}, Body: map[string]any{"a": 1}}, &out); err != nil || status != 200 || out["a"] != 1.0 {
		t.Fatalf("echo: %d %v %v", status, err, out)
	}
	var text string
	if _, err := Do(ctx, c, Request{Method: http.MethodPatch, URL: srv.URL + "/patch", Header: http.Header{"Content-Type": {"application/merge-patch+json"}}, Body: map[string]any{}}, &text); err != nil || text != "application/merge-patch+json" {
		t.Fatalf("own content type: %v %q", err, text)
	}
	if status, err := Do(ctx, c, Request{Method: http.MethodGet, URL: srv.URL + "/missing"}, nil); status != 404 {
		t.Fatalf("404: %d %v", status, err)
	} else if se, ok := errors.AsType[*StatusError](err); !ok || se.Status != 404 {
		t.Fatalf("404 not a StatusError: %v", err)
	}
	_, err := Do(ctx, c, Request{Method: http.MethodGet, URL: srv.URL + "/long"}, nil)
	if se, ok := errors.AsType[*StatusError](err); !ok || se.Status != 500 || len(se.Body) != 2048 {
		t.Fatalf("500 with a long body: %v", err)
	}
	_, err = Do(ctx, c, Request{Method: http.MethodGet, URL: srv.URL + "/long", ErrBody: 10}, nil)
	if se, _ := errors.AsType[*StatusError](err); se == nil || se.Body != strings.Repeat("x", 10) {
		t.Fatalf("ErrBody: %v", err)
	}
	if _, err := Do(ctx, c, Request{Method: http.MethodDelete, URL: srv.URL + "/empty", EmptyOK: true}, &out); err != nil {
		t.Fatalf("204 with EmptyOK: %v", err)
	}
	if _, err := Do(ctx, c, Request{Method: http.MethodDelete, URL: srv.URL + "/empty"}, &out); err == nil {
		t.Fatal("204 into a value without EmptyOK must fail")
	}
	if status, err := Do(ctx, c, Request{Method: http.MethodGet, URL: "http://127.0.0.1:1/"}, nil); err == nil || status != 0 {
		t.Fatalf("no answer: %d %v", status, err)
	}
}

func TestBasicAuth(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	req.SetBasicAuth("robot$envs", "s3cret")
	if got := BasicAuth("robot$envs", "s3cret"); got != req.Header.Get("Authorization") {
		t.Fatalf("%q, want %q", got, req.Header.Get("Authorization"))
	}
}
