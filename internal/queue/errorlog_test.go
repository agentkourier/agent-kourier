package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// A handler's error and a handler's panic value are the handler's own text, and may hold a secret or an event's
// content. Through courier's log handler the lines hold no secret-shaped value and no more than the cap, and keep the
// messages and keys operators grep.
func TestHandlerErrorsAndPanicsAreRedactedAndCappedInLogs(t *testing.T) {
	long := strings.Repeat("content of the alert payload. ", 40)
	planted := "password=hunter2-FAKE xoxb-1111-2222-FAKEFAKEFAKE Authorization: Bearer FAKE.bearer.credential " + long + "TAIL-CANARY"
	for _, tc := range []struct {
		name    string
		handler HandlerFunc
		msg     string
		level   string
		key     string
	}{
		{"an error", func(context.Context, store.Event) error { return errors.New("handle: " + planted) }, "handler failed", "WARN", "error"},
		{"a panic", func(context.Context, store.Event) error { panic(planted) }, "handler panicked", "ERROR", "panic"},
		{"a panic with an error", func(context.Context, store.Event) error { panic(errors.New(planted)) }, "handler panicked", "ERROR", "panic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			f := newFixture(t, tc.handler, func(c *Config) { c.Logger = slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)) })
			f.append(t, "ev")
			f.runOnce(t, true)

			for _, leak := range []string{"hunter2", "xoxb-1111", "FAKE.bearer.credential", "TAIL-CANARY"} {
				if strings.Contains(out.String(), leak) {
					t.Errorf("the log holds %q:\n%s", leak, out.String())
				}
			}
			var found bool
			for dec := json.NewDecoder(&out); dec.More(); {
				var r map[string]any
				if err := dec.Decode(&r); err != nil {
					t.Fatal(err)
				}
				if r["msg"] != tc.msg {
					continue
				}
				found = true
				text, _ := r[tc.key].(string)
				if r["level"] != tc.level || r["event"] != "ev" || r["attempt"] != float64(1) || !strings.Contains(text, "[redacted]") || len(text) > telemetry.MaxLogErrorBytes+20 {
					t.Errorf("line = %v; want level %s, event, attempt, and a redacted, capped %s", r, tc.level, tc.key)
				}
			}
			if !found {
				t.Errorf("no %q line:\n%s", tc.msg, out.String())
			}
		})
	}
}
