package gpu

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A GET whose connection breaks before the answer is tried again; a POST is not (it may have had its effect).
func TestRetryBrokenConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.(*net.TCPConn).SetLinger(0)
			conn.Close() // reset, no answer
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Start()
	defer srv.Close()
	c := RetryingClient(10 * time.Second)
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("GET: %v %d", err, calls.Load())
	}
	resp.Body.Close()
	calls.Store(0)
	if _, err := c.Post(srv.URL, "text/plain", nil); err == nil || calls.Load() != 1 {
		t.Fatalf("POST repeated: %v %d", err, calls.Load())
	}
}
