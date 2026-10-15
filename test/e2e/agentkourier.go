package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// maxMetrics is the most of a /metrics body that Counter reads.
const maxMetrics = 4 << 20

type auditPageResponse struct {
	Entries []store.AuditEntry `json:"entries"`
}

// ErrNoSample is wrapped by the error Counter returns when no sample has the name and labels. A
// counter that has not been incremented may not be exposed at all, so a caller that expects zero
// can tell that from a failure.
var ErrNoSample = errors.New("e2e: no such sample")

// AgentKourier reads Agent Kourier's own endpoints: /metrics and /readyz, on its health and metrics port.
type AgentKourier struct{ c *client }

// NewAgentKourier returns a client for the Agent Kourier at baseURL, with no default: the caller says where it
// is (Env.AgentKourierURL, once it has one).
func NewAgentKourier(baseURL string) *AgentKourier { return &AgentKourier{newClient(baseURL)} }

// WaitReady waits up to timeout for /readyz to answer 200. An answer of 503 (starting, or a
// dependency down) and a refused connection are waited out; the last one is in the timeout error,
// with what Agent Kourier said.
func (c *AgentKourier) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Wait(ctx, timeout, "Agent Kourier to be ready", func(ctx context.Context) (bool, error) {
		return true, c.c.do(ctx, http.MethodGet, "/readyz", nil, nil)
	})
}

// Counter scrapes /metrics and returns the value of the one sample named name whose labels include
// every one in labels (the sample may have more; none given matches any sample of the name, so a
// name with several label sets needs some). No such sample is ErrNoSample, and more than one is an
// error that says so: which of them is meant is not guessed. Any sample is read, a gauge too, though
// the helper is for counters. A body it cannot parse is an error, not a missing sample.
func (c *AgentKourier) Counter(ctx context.Context, name string, labels map[string]string) (float64, error) {
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

// AuditEntries reads the persisted audit rows exposed by the testhooks image.
func (c *AgentKourier) AuditEntries(ctx context.Context) ([]store.AuditEntry, error) {
	var all []store.AuditEntry
	var afterSeq int64
	for {
		page, err := c.auditPage(ctx, afterSeq)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		for i, entry := range page {
			if entry.Sequence <= afterSeq || (i > 0 && entry.Sequence <= page[i-1].Sequence) {
				return nil, fmt.Errorf("e2e: audit page cursor did not advance past %d", afterSeq)
			}
		}
		all = append(all, page...)
		afterSeq = page[len(page)-1].Sequence
	}
}

func (c *AgentKourier) auditPage(ctx context.Context, afterSeq int64) ([]store.AuditEntry, error) {
	path := "/__testhooks/audit"
	if afterSeq > 0 {
		path += "?after_seq=" + strconv.FormatInt(afterSeq, 10)
	}
	var raw json.RawMessage
	if err := c.c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("e2e: audit response must be an object")
	}
	var response auditPageResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("e2e: decode audit page: %w", err)
	}
	if response.Entries == nil {
		return nil, errors.New("e2e: audit page has no entries")
	}
	return response.Entries, nil
}

// getText returns the body of a 2xx answer to GET path, up to limit bytes (more is an error).
func (c *client) getText(ctx context.Context, path string, limit int64) (string, error) {
	b, err := c.exchange(ctx, http.MethodGet, path, nil, false, limit)
	return string(b), err
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

// parseLabels reads name="value" pairs up to the closing brace, and returns them and what follows. The
// pairs are separated by commas, with one allowed after the last, and a name is a label name that
// appears once; a quoted name, which newer exposition formats allow and Agent Kourier does not write, is an
// error rather than a guess.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	s = strings.TrimLeft(s, " ")
	if rest, ok := strings.CutPrefix(s, "}"); ok {
		return labels, rest, nil
	}
	for {
		eq := strings.Index(s, `="`)
		if eq <= 0 || !labelName(s[:eq]) {
			return nil, "", errors.New(`labels: want name="value", with a name of letters, digits and _`)
		}
		key := s[:eq]
		if _, dup := labels[key]; dup {
			return nil, "", fmt.Errorf("label %s: given twice", key)
		}
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
		s = strings.TrimLeft(s[i+1:], " ")
		if rest, ok := strings.CutPrefix(s, "}"); ok {
			return labels, rest, nil
		}
		rest, ok := strings.CutPrefix(s, ",")
		if !ok {
			return nil, "", fmt.Errorf("label %s: want a comma or the closing brace after it", key)
		}
		s = rest
		if s = strings.TrimLeft(s, " "); strings.HasPrefix(s, "}") { // a comma after the last pair
			return labels, s[1:], nil
		}
	}
}

// labelName reports whether s is a label name: a letter or _, then letters, digits and _.
func labelName(s string) bool {
	for i, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}
