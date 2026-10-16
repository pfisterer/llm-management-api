package gpu

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// retryDNS wraps a transport and repeats a request that failed on the way out (up to three attempts, 1 s and 2 s apart). The DNS servers in front of both clusters have shown short outages ("no such host", "server misbehaving") for names that resolve again a second later; any request is repeated for those. GET and HEAD requests are also repeated after a connection that broke before an answer (TLS handshake timeout, connection reset, EOF): calls from the workspace pods to GitHub and quay.io have failed that way in short bursts (16.10.2026), while the same calls a moment later went through. Only requests without a body or with a replayable body (GetBody) are repeated.
type retryDNS struct{ next http.RoundTripper }

func (r retryDNS) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-req.Context().Done():
				return resp, err
			}
			if req.Body != nil {
				if req.GetBody == nil {
					return resp, err
				}
				body, berr := req.GetBody()
				if berr != nil {
					return resp, err
				}
				req.Body = body
			}
		}
		resp, err = r.next.RoundTrip(req)
		if err == nil || !retryable(req, err) {
			return resp, err
		}
	}
	return resp, err
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

func withDNSRetry(c *http.Client) *http.Client {
	next := c.Transport
	if next == nil {
		next = transport()
	}
	c.Transport = retryDNS{next: next}
	return c
}

// RetryingClient is an HTTP client with these retries, for other parts of the service (the registry checks of finished images).
func RetryingClient(timeout time.Duration) *http.Client {
	return withDNSRetry(&http.Client{Timeout: timeout})
}
