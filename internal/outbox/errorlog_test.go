package outbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// A post the platform refuses with a body slack-go cannot parse is abandoned, and the warning carries the error:
// "unknown error structure: <the raw body>". Through Agent Kourier's log handler the line holds no secret-shaped value
// and no more of the body than the cap, and keeps the message, the reason and the keys operators grep.
func TestAbandonWarningRedactsAndCapsTheErrorBody(t *testing.T) {
	long := strings.Repeat("text of a private message. ", 40)
	body := `{"ok":false,"errors":[{"detail":"password=hunter2-FAKE xoxb-1111-2222-FAKEFAKEFAKE ` + long + `TAIL-CANARY"}]}`
	var parsed slackgo.SlackResponse
	parseErr := json.Unmarshal([]byte(body), &parsed)
	if parseErr == nil || !strings.Contains(parseErr.Error(), "unknown error structure") {
		t.Fatalf("slack-go parsed the body (%v); want its unknown error structure, the case under test", parseErr)
	}
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{onPost: func(context.Context) error { return fmt.Errorf("post: %w: %w", chat.ErrInvalid, parseErr) }}
	logs := &syncBuffer{}
	ob, _, _ := abandoning(t, st, p, func(c *outbox.Config) { c.Logger = slog.New(telemetry.NewJSONHandler(logs, slog.LevelDebug)) })
	if _, err := ob.Send(t.Context(), reply("ev1")); err == nil {
		t.Fatal("Send of a refused post succeeded")
	}

	out := logs.String()
	for _, leak := range []string{"hunter2", "xoxb-1111", "TAIL-CANARY"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log holds %q:\n%s", leak, out)
		}
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil {
		t.Fatalf("log %q: %v", out, err)
	}
	text, _ := line["err"].(string)
	if line["msg"] != "outbox: a post was abandoned and will not be retried" || line["level"] != "WARN" || line["outbox_id"] != "ev1/reply" || line["reason"] != outbox.AbandonInvalid {
		t.Errorf("line = %v; want the message, level and keys operators grep", line)
	}
	if !strings.Contains(text, "unknown error structure") || !strings.Contains(text, "[redacted]") || len(text) > telemetry.MaxLogErrorBytes+20 {
		t.Errorf("err = %q; want the class of failure, a redaction and the cap", text)
	}
}
