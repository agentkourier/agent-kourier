package slack_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// refusingClient returns a Client over a Slack whose every method answers ok:false with code.
func refusingClient(t *testing.T, code string) *slack.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
	}))
	t.Cleanup(srv.Close)
	return slack.New(slack.Config{BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: srv.URL + "/",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// A Slack error code is the server's text. Slack's own codes are a closed set, but a proxy or a
// test double can say anything, and the error holds whatever scrub left of it: the span and the
// metric get the outcome class, and never the error.
func TestChatErrorsAreNotExported(t *testing.T) {
	const canary = "leakcanary"
	h := telemetrytest.New(t)
	c := refusingClient(t, canary+"_"+botToken)
	ctx := h.Ctx
	_, errPost := c.Post(ctx, "C1", "", slack.Content{Text: "private-message-text"}, "ob-1")
	errAppend := c.AppendStream(ctx, "C1", "1.0", "private-stream-text", slack.ChunksMode)
	errStep := c.AppendStep(ctx, "C1", "1.0", slack.TaskUpdate{ID: "s1", Title: "private-step-title", Status: "in_progress"})
	for _, err := range []error{errPost, errAppend, errStep} {
		if err == nil || !strings.Contains(err.Error(), canary) {
			t.Fatalf("err = %v; want the failure, which holds the code the test must keep out of telemetry", err)
		}
	}
	h.RequireNone(t, canary, botToken, appToken, "private-message-text", "private-stream-text", "private-step-title")
	for _, op := range []string{"Post", "AppendStream", "AppendStep"} {
		if h.Counter(t, "agentkourier_chat_api_calls_total", map[string]string{"operation": op, "outcome": "api_error"}) != 1 {
			t.Errorf("%s was not counted once as an api_error", op)
		}
	}
	for _, s := range h.Exporter.GetSpans() {
		if s.Status.Description != "api_error" {
			t.Errorf("span %s: status description %q, want the bounded outcome", s.Name, s.Status.Description)
		}
	}
}

func TestRefusedChunksModeIsItsOwnOutcome(t *testing.T) {
	for _, code := range []string{"invalid_chunks", "streaming_mode_mismatch", "cannot_provide_both_markdown_text_and_chunks"} {
		t.Run(code, func(t *testing.T) {
			h := telemetrytest.New(t)
			err := refusingClient(t, code).AppendStream(h.Ctx, "C1", "1.0", "text", slack.ChunksMode)
			if err == nil {
				t.Fatal("AppendStream succeeded")
			}
			if h.Counter(t, "agentkourier_chat_api_calls_total", map[string]string{"operation": "AppendStream", "outcome": "mode_refused"}) != 1 {
				t.Errorf("%s was not counted as mode_refused: %s", code, h.Exported(t))
			}
			if s := h.Span(t, "agentkourier.chat.AppendStream"); s.Status.Description != "mode_refused" {
				t.Errorf("span status = %q, want mode_refused", s.Status.Description)
			}
		})
	}
	// Other API errors, and the arguments error that also starts the fallback, stay api_error.
	h := telemetrytest.New(t)
	_ = refusingClient(t, "invalid_arguments").AppendStream(h.Ctx, "C1", "1.0", "text", slack.ChunksMode)
	if h.Counter(t, "agentkourier_chat_api_calls_total", map[string]string{"operation": "AppendStream", "outcome": "api_error"}) != 1 {
		t.Error("invalid_arguments is not an api_error")
	}
}
