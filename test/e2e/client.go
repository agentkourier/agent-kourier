package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	httpTimeout = 10 * time.Second
	maxBody     = 1 << 20 // the most of a response body a helper reads
	maxErrBody  = 512     // the most of an error response a StatusError keeps
)

// StatusError is a non-2xx answer from a double.
type StatusError struct {
	Method, URL string
	Status      int
	Body        string // at most maxErrBody bytes
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.Status, strings.TrimSpace(e.Body))
}

// client is the JSON-over-HTTP caller that the helpers for one double share.
type client struct {
	base string
	hc   *http.Client
}

func newClient(base string) *client {
	return &client{strings.TrimRight(base, "/"), &http.Client{Timeout: httpTimeout}}
}

// do sends in (nil for no body) as JSON and decodes a 2xx answer into out (nil to ignore it). Any
// other status is a *StatusError; a failure to get an answer is the transport's error.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("e2e: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("e2e: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("e2e: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return &StatusError{method, req.URL.String(), resp.StatusCode, string(msg)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("e2e: decode %s %s: %w", method, path, err)
	}
	return nil
}
