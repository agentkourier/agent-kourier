package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// The text of an agent's JSON-RPC error is the agent's own: kagent puts the exception of a failed model call or tool
// in it, and that may quote model output, a tool's input or a credential. It reaches the ERROR and WARN lines of a
// failed turn through the handler Agent Kourier is built with, which redacts it and cuts it, so the line names the turn
// and the head of the cause and holds no secret and no page of text. Message and keys stay the ones operators grep.
func TestAgentErrorTextIsRedactedAndCappedInTurnFailureLogs(t *testing.T) {
	long := strings.Repeat("the model quoted the customer's record. ", 40)
	rpc := a2a.NewError(a2a.ErrInternalError, "litellm.AuthenticationError: bad key xoxb-1111-2222-FAKEFAKEFAKE, "+
		"Authorization: Bearer FAKE.bearer.credential, password=hunter2-FAKE. "+long+"TAIL-CANARY")
	for _, tc := range []struct {
		name  string
		err   error
		level string
		keys  []string
	}{
		// log(): a send that failed in any way but the ones with their own handling.
		{"a failed send", fmt.Errorf("send message: %w", rpc), "ERROR", []string{"key", "error"}},
		// fail(): the agent refused the Binding's identity, a turn that ends with a notice.
		{"a refused identity", fmt.Errorf("send message: %w: %w", agent.ErrForbidden, rpc), "WARN", []string{"key", "message", "reason", "error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var out lockedBuf
			f.m.cfg.Logger = slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug))
			f.agent.script = func(context.Context, int, agent.Handler) error { return tc.err }
			f.live()
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the failure line", func() bool { return strings.Contains(out.String(), "session turn failed") })
			f.retired(t)

			all := out.String()
			for _, leak := range []string{"xoxb-1111", "FAKE.bearer.credential", "hunter2", "TAIL-CANARY"} {
				if strings.Contains(all, leak) {
					t.Errorf("the log holds %q:\n%s", leak, all)
				}
			}
			var line map[string]any
			for _, l := range strings.Split(strings.TrimSpace(all), "\n") {
				var m map[string]any
				if err := json.Unmarshal([]byte(l), &m); err == nil && m["msg"] == "session turn failed" {
					line = m
				}
			}
			if line == nil || line["level"] != tc.level {
				t.Fatalf("no %s line \"session turn failed\" in:\n%s", tc.level, all)
			}
			for _, k := range tc.keys {
				if _, ok := line[k]; !ok {
					t.Errorf("the line has no %q: %v", k, line)
				}
			}
			if text, _ := line["error"].(string); !strings.Contains(text, "send message") || !strings.Contains(text, "[redacted]") || len(text) > telemetry.MaxLogErrorBytes+20 {
				t.Errorf("error = %q; want the context of the call, a redaction and the cap", text)
			}
		})
	}
}
