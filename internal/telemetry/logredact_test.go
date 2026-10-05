package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// Fake credentials, shaped like the real thing.
const (
	fakeSlackToken = "xoxb-1111-2222-FAKEFAKEFAKE"
	fakeBearer     = "Bearer FAKE.bearer.credential"
	fakePassword   = "password=hunter2-FAKE"
)

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg } // dereferences: a typed nil panics

// logLine decodes the one JSON line the logger wrote.
func logLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("log output %q is no one JSON line: %v", buf.String(), err)
	}
	return m
}

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(NewJSONHandler(buf, slog.LevelDebug))
}

// secrets and the long content never reach the line, in whichever shape the error attribute comes.
func TestJSONHandlerRedactsErrorAttributes(t *testing.T) {
	long := strings.Repeat("model output that must not be logged whole. ", 40)
	agentErr := fmt.Errorf("send message: %w", errors.New(fakeSlackToken+" "+fakeBearer+" "+fakePassword+" "+long+"TAIL-CANARY"))
	for _, tc := range []struct {
		name string
		log  func(l *slog.Logger)
		key  string // where the value is, as a path
	}{
		{"error key", func(l *slog.Logger) { l.Error("m", "error", agentErr) }, "error"},
		{"err key", func(l *slog.Logger) { l.Warn("m", "err", agentErr) }, "err"},
		{"cause key, a string", func(l *slog.Logger) { l.Warn("m", "cause", agentErr.Error()) }, "cause"},
		{"reason key, a string", func(l *slog.Logger) { l.Info("m", "reason", agentErr.Error()) }, "reason"},
		{"reason key, an error", func(l *slog.Logger) { l.Info("m", "reason", agentErr) }, "reason"},
		{"panic key, any value", func(l *slog.Logger) { l.Error("m", "panic", agentErr.Error()) }, "panic"},
		{"panic key, an error value", func(l *slog.Logger) { l.Error("m", "panic", agentErr) }, "panic"},
		{"an error under any key", func(l *slog.Logger) { l.Error("m", "whatever", agentErr) }, "whatever"},
		{"slog.Any", func(l *slog.Logger) { l.Error("m", slog.Any("failure", agentErr)) }, "failure"},
		{"context variant", func(l *slog.Logger) { l.ErrorContext(t.Context(), "m", "err", agentErr) }, "err"},
		{"With", func(l *slog.Logger) { l.With("error", agentErr).Error("m") }, "error"},
		{"WithGroup", func(l *slog.Logger) { l.WithGroup("g").Error("m", "error", agentErr) }, "g.error"},
		{"With inside WithGroup", func(l *slog.Logger) { l.WithGroup("g").With("err", agentErr).Error("m") }, "g.err"},
		{"slog.Group", func(l *slog.Logger) { l.Error("m", slog.Group("g", slog.Any("error", agentErr))) }, "g.error"},
		{"nested groups", func(l *slog.Logger) { l.Error("m", slog.Group("a", slog.Group("b", "cause", agentErr))) }, "a.b.cause"},
		{"a joined error", func(l *slog.Logger) { l.Error("m", "error", errors.Join(agentErr, errors.New("second"))) }, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(newTestLogger(&buf))
			out := buf.String()
			for _, leak := range []string{fakeSlackToken, "FAKE.bearer.credential", "hunter2", "TAIL-CANARY"} {
				if strings.Contains(out, leak) {
					t.Errorf("the line holds %q:\n%s", leak, out)
				}
			}
			got := fmt.Sprint(lookup(logLine(t, &buf), tc.key))
			if !strings.Contains(got, "[redacted]") {
				t.Errorf("%s = %q; want the redacted text, not an empty value", tc.key, got)
			}
			if len(got) > MaxLogErrorBytes+len(redact.Truncated) {
				t.Errorf("%s is %d bytes; the cap is %d", tc.key, len(got), MaxLogErrorBytes)
			}
		})
	}
}

// lookup follows a dotted path through the decoded line.
func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

// What an operator greps stays: the message, the level, the time and every key of every attribute, and a short
// error that holds nothing secret is not changed at all.
func TestJSONHandlerKeepsMessagesAndKeys(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf)
	l.Error("session turn failed", "key", "ns/b1", "message", "m-1", "reason", "the agent could not be reached", "error", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), "attempt", 3)
	m := logLine(t, &buf)
	want := map[string]any{
		"msg": "session turn failed", "level": "ERROR", "key": "ns/b1", "message": "m-1", "attempt": float64(3),
		"reason": "the agent could not be reached", "error": "dial tcp 10.0.0.1:443: connect: connection refused",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v; want %v", k, m[k], v)
		}
	}
	if _, ok := m["time"]; !ok || len(m) != len(want)+1 {
		t.Errorf("keys = %v; want %d keys and the time", m, len(want))
	}
}

// Text that is not an error attribute is left alone: the handler does not touch what no one asked it to.
func TestJSONHandlerLeavesOtherAttributes(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).Info("msg with "+fakePassword, "channel", "C1", "detail", fakePassword, "n", 7, "ok", true)
	m := logLine(t, &buf)
	if m["detail"] != fakePassword || m["channel"] != "C1" || m["n"] != float64(7) || m["ok"] != true || m["msg"] != "msg with "+fakePassword {
		t.Errorf("line = %v; want the other attributes untouched", m)
	}
}

// The cap cuts on a rune boundary, marks the cut, and applies after the redaction: a secret that sat at the cut is
// gone, and a multi-byte rune is never split.
func TestJSONHandlerCapsAtARuneBoundary(t *testing.T) {
	text := strings.Repeat("é", MaxLogErrorBytes) // two bytes each
	var buf bytes.Buffer
	newTestLogger(&buf).Error("m", "error", errors.New(text))
	got, _ := logLine(t, &buf)["error"].(string)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, redact.Truncated) {
		t.Fatalf("error = %q; want valid UTF-8 ending in the cut mark", got)
	}
	if body := strings.TrimSuffix(got, redact.Truncated); len(body) > MaxLogErrorBytes || len(body) < MaxLogErrorBytes-3 {
		t.Errorf("the kept text is %d bytes; want up to %d", len(body), MaxLogErrorBytes)
	}
	// A Basic credential that starts 13 bytes before the cut: cut first, what is left is "Basic dXNlcjp", too short
	// for the redaction to know, and a third of the credential is logged. Redacted first, none of it is.
	const credential = "dXNlcjpwYXNzd29yZA=="
	var at bytes.Buffer
	newTestLogger(&at).Error("m", "error", errors.New(strings.Repeat("x ", 142)+"xx Basic "+credential+" and the rest of a long message "+strings.Repeat("y ", 100)))
	if out := at.String(); strings.Contains(out, credential[:7]) || !strings.Contains(out, "[redacted]") {
		t.Errorf("the credential at the cut was not redacted before the cut:\n%s", out)
	}
}

// An error that cannot print, or no error at all, does not take the logger down.
func TestJSONHandlerSurvivesBadErrors(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf)
	var typedNil *nilErr
	l.Error("a", "error", typedNil)
	l.Error("b", "error", nil)
	l.Error("c", "err", error(nil))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s; want 3", len(lines), buf.String())
	}
}

// Courier builds a log handler in one place. A second JSON or text handler, in any non-test file of cmd or
// internal, is a logger the redaction does not cover.
func TestOnlyTheTelemetryPackageBuildsHandlers(t *testing.T) {
	const root = "../.."
	var found []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if s := string(b); strings.Contains(s, "slog.NewJSONHandler(") || strings.Contains(s, "slog.NewTextHandler(") {
				found = append(found, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) != 1 || !strings.HasSuffix(found[0], "internal/telemetry/log.go") {
		t.Errorf("handlers are built in %v; want only internal/telemetry/log.go", found)
	}
}
