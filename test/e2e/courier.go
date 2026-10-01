package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxMetrics is the most of a /metrics body that Counter reads.
const maxMetrics = 4 << 20

// ErrNoSample is wrapped by the error Counter returns when no sample has the name and labels. A
// counter that has not been incremented may not be exposed at all, so a caller that expects zero
// can tell that from a failure.
var ErrNoSample = errors.New("e2e: no such sample")

// Courier reads courier's own endpoints: /metrics and /readyz, on its health and metrics port.
type Courier struct{ c *client }

// NewCourier returns a client for the courier at baseURL, with no default: the caller says where it
// is (Env.CourierURL, once it has one).
func NewCourier(baseURL string) *Courier { return &Courier{newClient(baseURL)} }

// WaitReady waits up to timeout for /readyz to answer 200. An answer of 503 (starting, or a
// dependency down) and a refused connection are waited out; the last one is in the timeout error,
// with what courier said.
func (c *Courier) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Wait(ctx, timeout, "courier to be ready", func(ctx context.Context) (bool, error) {
		return true, c.c.do(ctx, http.MethodGet, "/readyz", nil, nil)
	})
}

// Counter scrapes /metrics and returns the value of the one sample named name whose labels include
// every one in labels (the sample may have more; none given matches any sample of the name, so a
// name with several label sets needs some). No such sample is ErrNoSample, and more than one is an
// error that says so: which of them is meant is not guessed. Any sample is read, a gauge too, though
// the helper is for counters. A body it cannot parse is an error, not a missing sample.
func (c *Courier) Counter(ctx context.Context, name string, labels map[string]string) (float64, error) {
	body, err := c.c.getText(ctx, "/metrics", maxMetrics)
	if err != nil {
		return 0, err
	}
	var found []float64
	for n, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		got, have, value, err := parseSample(line)
		if err != nil {
			return 0, fmt.Errorf("e2e: /metrics line %d: %w", n+1, err)
		}
		if got == name && includes(have, labels) {
			found = append(found, value)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("%w: %s%v", ErrNoSample, name, labels)
	case 1:
		return found[0], nil
	}
	return 0, fmt.Errorf("e2e: %d samples of %s match %v; add labels to say which", len(found), name, labels)
}

// getText returns the body of a 2xx answer to GET path, up to limit bytes (more is an error).
func (c *client) getText(ctx context.Context, path string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", fmt.Errorf("e2e: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2e: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return "", &StatusError{http.MethodGet, req.URL.String(), resp.StatusCode, string(msg)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("e2e: read GET %s: %w", path, err)
	case int64(len(b)) > limit:
		return "", fmt.Errorf("e2e: GET %s: %w: over %d bytes", path, errTooLarge, limit)
	}
	return string(b), nil
}

// includes reports whether have holds every label in want.
func includes(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// parseSample reads one line of the Prometheus text format: name, optional {labels}, a value, and
// an optional timestamp, which is ignored.
func parseSample(line string) (name string, labels map[string]string, value float64, err error) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return "", nil, 0, fmt.Errorf("no value in %q", line)
	}
	name, rest := line[:i], line[i:]
	labels = map[string]string{}
	if rest[0] == '{' {
		if labels, rest, err = parseLabels(rest[1:]); err != nil {
			return "", nil, 0, fmt.Errorf("%s: %w", name, err)
		}
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 {
		return "", nil, 0, fmt.Errorf("%s: want a value and at most a timestamp, got %q", name, rest)
	}
	if value, err = strconv.ParseFloat(fields[0], 64); err != nil {
		return "", nil, 0, fmt.Errorf("%s: value: %w", name, err)
	}
	return name, labels, value, nil
}

// parseLabels reads name="value" pairs up to the closing brace, and returns them and what follows.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ")
		if rest, ok := strings.CutPrefix(s, "}"); ok {
			return labels, rest, nil
		}
		eq := strings.Index(s, `="`)
		if eq <= 0 {
			return nil, "", errors.New(`labels: want name="value"`)
		}
		key := s[:eq]
		var b strings.Builder
		i := eq + 2
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] != '\\' {
				b.WriteByte(s[i])
				continue
			}
			if i++; i == len(s) {
				break
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case '\\', '"':
				b.WriteByte(s[i])
			default:
				return nil, "", fmt.Errorf(`label %s: no escape \%c`, key, s[i])
			}
		}
		if i >= len(s) {
			return nil, "", fmt.Errorf("label %s: the value does not end", key)
		}
		labels[key] = b.String()
		s = strings.TrimPrefix(strings.TrimLeft(s[i+1:], " "), ",")
	}
}
