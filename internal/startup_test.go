package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestStartupServerAnswersNotReady(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	hs := startupServer(addr, zap.NewNop().Sugar())
	defer hs.Shutdown(context.Background())
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get("http://" + addr + "/health"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "starting") {
		t.Fatalf("got %d %s, want 503 starting", resp.StatusCode, body)
	}
	if err := hs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The address is free again for the real listener.
	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("address not released: %v", err)
	}
	l2.Close()
}
