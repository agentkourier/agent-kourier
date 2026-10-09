package slack_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// A Web API response slack-go cannot parse comes back as "unknown error structure: <the raw body>", and the body may
// quote what was sent or what a workspace holds. The error Agent Kourier's callers log carries the tokens scrubbed, and,
// once the handler redacts and cuts it, no secret-shaped value and no more of the body than the cap. The message and
// the key of the line stay as the callers set them.
func TestUnknownSlackErrorBodyIsRedactedAndCappedInLogs(t *testing.T) {
	long := strings.Repeat("a message from a private channel. ", 40)
	c, calls, _ := userClient(t, nil)
	calls.status = http.StatusOK
	calls.body = `{"ok":false,"errors":[{"detail":"password=hunter2-FAKE Authorization: Bearer FAKE.bearer.credential ` + long + `TAIL-CANARY"}]}`
	_, err := c.ResolveUser(t.Context(), "U1")
	if err == nil || !strings.Contains(err.Error(), "unknown error structure") {
		t.Fatalf("ResolveUser = %v; want slack-go's unknown error structure, the case under test", err)
	}

	var out bytes.Buffer
	slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)).Warn("slack: a call failed", "error", err)
	got := out.String()
	for _, leak := range []string{"hunter2", "FAKE.bearer.credential", "TAIL-CANARY"} {
		if strings.Contains(got, leak) {
			t.Errorf("the line holds %q:\n%s", leak, got)
		}
	}
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	text, _ := line["error"].(string)
	if line["msg"] != "slack: a call failed" || !strings.Contains(text, "unknown error structure") || !strings.Contains(text, "[redacted]") || len(text) > telemetry.MaxLogErrorBytes+20 {
		t.Errorf("line = %v; want the message, the class of failure in the error, a redaction and the cap", line)
	}
}
