package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// errTooLarge is wrapped by the error for a 2xx answer longer than the client's limit.
var errTooLarge = errors.New("response too large")

// client is the JSON-over-HTTP caller that the helpers for one double share.
type client struct {
	base  string
	hc    *http.Client
	limit int64 // the most of an answer do reads; maxBody unless a helper needs more
}

func newClient(base string) *client {
	return &client{strings.TrimRight(base, "/"), &http.Client{Timeout: httpTimeout}, maxBody}
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
	var limit int64 // an answer that is ignored is not read
	if out != nil {
		limit = c.limit
	}
	b, err := c.exchange(ctx, method, path, body, in != nil, limit)
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("e2e: decode %s %s: %w", method, path, err)
	}
	return nil
}

// exchange is the one place a request is made and its answer checked, for the JSON calls and the text
// ones: it sends body (nil for none, as JSON if asJSON is set), turns a status outside 2xx into a
// *StatusError, and returns the answer's body, up to limit bytes (more is errTooLarge). A limit of
// zero reads nothing, for an answer the caller ignores.
func (c *client) exchange(ctx context.Context, method, path string, body io.Reader, asJSON bool, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("e2e: %w", err)
	}
	if asJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2e: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, &StatusError{method, req.URL.String(), resp.StatusCode, string(msg)}
	}
	if limit <= 0 {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("e2e: read %s %s: %w", method, path, err)
	case int64(len(b)) > limit:
		return nil, fmt.Errorf("e2e: %s %s: %w: over %d bytes", method, path, errTooLarge, limit)
	}
	return b, nil
}
