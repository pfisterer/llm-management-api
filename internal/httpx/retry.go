// Package httpx holds the service's HTTP clients: every client gets a time limit, and the ones that talk to other hosts repeat a request that failed on the way out.
package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/retry"
)

// attempts is how often a request is tried at most; the pauses are 1 s and 2 s (retry.Do doubles from 1 s, at most 2 s).
const attempts = 3

var policy = retry.Policy{Total: time.Hour, Initial: time.Second, Max: 2 * time.Second} // Total is not the limit: attempts and the request's context are

// retrying wraps a transport and repeats a request that failed on the way out (up to three attempts, 1 s and 2 s apart). The DNS servers in front of both clusters have shown short outages ("no such host", "server misbehaving") for names that resolve again a second later; any request is repeated for those. GET and HEAD requests are also repeated after a connection that broke before an answer (TLS handshake timeout, connection reset, EOF): calls from the workspace pods to GitHub and quay.io have failed that way in short bursts (16.10.2026), while the same calls a moment later went through. Only requests without a body or with a replayable body (GetBody) are repeated.
type retrying struct{ next http.RoundTripper }

func (r retrying) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var last error
	attempt := 0
	err := retry.Do(req.Context(), policy, func(context.Context) error {
		attempt++
		if attempt > 1 && req.Body != nil {
			if req.GetBody == nil {
				return retry.Permanent(last)
			}
			body, err := req.GetBody()
			if err != nil {
				return retry.Permanent(last)
			}
			req.Body = body
		}
		resp, last = r.next.RoundTrip(req)
		if last != nil && (attempt == attempts || !retryable(req, last)) {
			return retry.Permanent(last)
		}
		return last
	})
	if attempt == 0 { // the context ended before the first attempt
		return nil, err
	}
	return resp, last
}

// retryable tells whether a failed request is worth another attempt: a name that did not resolve, or, for a request without side effects, a connection that broke before the answer.
func retryable(req *http.Request, err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if req.Context().Err() != nil {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "TLS handshake timeout")
}

// transport is the default transport with a shorter TLS handshake timeout, so a stuck handshake leaves room for another attempt within a client's timeout.
func transport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 5 * time.Second
	return t
}

// WithRetry makes c repeat requests that failed on the way out (see retrying); a client without a transport of its own gets the default one with a 5 s TLS handshake limit.
func WithRetry(c *http.Client) *http.Client {
	next := c.Transport
	if next == nil {
		next = transport()
	}
	c.Transport = retrying{next: next}
	return c
}

// NewClient is a client with the time limit timeout for a whole request, answer included, on the default transport.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// RetryingClient is a client with the time limit timeout that repeats requests that failed on the way out (WithRetry).
func RetryingClient(timeout time.Duration) *http.Client {
	return WithRetry(NewClient(timeout))
}
