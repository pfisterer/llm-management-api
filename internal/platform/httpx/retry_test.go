package httpx

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

type failing struct {
	calls int
	err   error
}

func (f *failing) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, f.err
}

// A name that does not resolve is tried three times, for any method with a replayable body; a body that cannot be sent again is sent once.
func TestRetryDNSAttempts(t *testing.T) {
	if policy.Attempts != 3 || policy.Initial != time.Second || policy.Max != 2*time.Second {
		t.Fatalf("policy %+v, want three attempts 1 s and 2 s apart", policy)
	}
	saved := policy
	policy.Initial, policy.Max = time.Millisecond, 2*time.Millisecond
	defer func() { policy = saved }()
	dnsErr := &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}
	f := &failing{err: dnsErr}
	c := &http.Client{Transport: retrying{next: f}}
	if _, err := c.Get("http://x.invalid/"); err == nil || f.calls != policy.Attempts {
		t.Fatalf("GET: %v after %d calls, want %d", err, f.calls, policy.Attempts)
	}
	f.calls = 0
	if _, err := c.Post("http://x.invalid/", "text/plain", strings.NewReader("a")); err == nil || f.calls != policy.Attempts {
		t.Fatalf("POST with a replayable body: %v after %d calls", err, f.calls)
	}
	f.calls = 0
	req, _ := http.NewRequest(http.MethodPost, "http://x.invalid/", io.NopCloser(strings.NewReader("a")))
	if _, err := c.Do(req); err == nil || f.calls != 1 {
		t.Fatalf("POST without GetBody: %v after %d calls, want 1", err, f.calls)
	}
	f.calls = 0
	f.err = errors.New("connection refused")
	if _, err := c.Get("http://x.invalid/"); err == nil || f.calls != 1 {
		t.Fatalf("not retryable: %v after %d calls, want 1", err, f.calls)
	}
}
