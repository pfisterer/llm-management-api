package httpx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Request is one call of a JSON API.
type Request struct {
	Method string
	URL    string
	// Header is set on the request (authorization, Accept, a Content-Type other than JSON).
	Header http.Header
	// Body, when not nil, is sent as JSON; Content-Type is application/json unless Header names another.
	Body any
	// MaxBody is the most of a 2xx answer that is read; 0 = all of it.
	MaxBody int64
	// ErrBody is the most of an answer outside 2xx that is kept in StatusError.Body; 0 = 2 KiB.
	ErrBody int64
	// EmptyOK leaves out as it is when a 2xx answer has no body (or only white space) instead of failing.
	EmptyOK bool
}

// StatusError is an answer outside 2xx: its status and the start of its body (Request.ErrBody).
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Do sends r with c and decodes a 2xx answer into out: nil = the answer is read and dropped, *string = the answer as text, anything else = JSON. It returns the status (0 without an answer); an answer outside 2xx is a *StatusError, an error of the client is returned as it is, for the caller to name its service.
func Do(ctx context.Context, c *http.Client, r Request, out any) (int, error) {
	var rd io.Reader
	if r.Body != nil {
		b, err := json.Marshal(r.Body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, rd)
	if err != nil {
		return 0, err
	}
	for k, v := range r.Header {
		req.Header[k] = v
	}
	if r.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		limit := r.ErrBody
		if limit <= 0 {
			limit = 2 << 10
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, Body: string(msg)}
	}
	var body io.Reader = resp.Body
	if r.MaxBody > 0 {
		body = io.LimitReader(resp.Body, r.MaxBody)
	}
	switch o := out.(type) {
	case nil:
		_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
		return resp.StatusCode, nil
	case *string:
		b, err := io.ReadAll(body)
		*o = string(b)
		return resp.StatusCode, err
	}
	if r.EmptyOK {
		raw, err := io.ReadAll(body)
		if err != nil {
			return resp.StatusCode, err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, json.Unmarshal(raw, out)
	}
	return resp.StatusCode, json.NewDecoder(body).Decode(out)
}

// BasicAuth is the value of an Authorization header for HTTP basic authentication, as http.Request.SetBasicAuth sets it.
func BasicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}
