package gpu

import (
	"errors"
	"net"
	"net/http"
	"time"
)

// retryDNS wraps a transport and repeats a request whose host name did not
// resolve (up to three attempts, 1 s apart). The DNS servers in front of both
// clusters have shown short outages ("no such host", "server misbehaving")
// for names that resolve again a second later. Only requests without a body
// or with a replayable body (GetBody) are repeated.
type retryDNS struct{ next http.RoundTripper }

func (r retryDNS) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
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
		var dnsErr *net.DNSError
		if err == nil || !errors.As(err, &dnsErr) {
			return resp, err
		}
	}
	return resp, err
}

func withDNSRetry(c *http.Client) *http.Client {
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = retryDNS{next: next}
	return c
}
