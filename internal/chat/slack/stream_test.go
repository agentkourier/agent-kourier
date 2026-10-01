package slack_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// updateClient returns a Client over a fake Slack, whose requests the counter sees, and the
// ts of a message in C1 for a thread.
func updateClient(t *testing.T) (c *slack.Client, fake *fakeslack.Server, calls *counter, root string) {
	t.Helper()
	fake, calls = newFake(t), &counter{n: map[string]int{}}
	cfg := clientConfig(fake)
	cfg.HTTPClient = &http.Client{Transport: calls}
	_, root, err := slackgo.New(botToken, slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("root", false))
	if err != nil {
		t.Fatal(err)
	}
	return slack.New(cfg), fake, calls, root
}

func TestStreamLifecycle(t *testing.T) {
	c, fake, _, root := updateClient(t)
	reply := func() fakeslack.Message { th := fake.Thread("C1", root); return th[len(th)-1] }
	ts, err := c.StartStream(t.Context(), "C1", root, "U1", "Hel")
	if err != nil {
		t.Fatal(err)
	}
	if m := reply(); m.TS != ts || m.ThreadTS != root || m.Text != "Hel" || !m.Streaming {
		t.Fatalf("after StartStream: %+v; want a streaming reply in the thread with the first text", m)
	}
	if err := c.AppendStream(t.Context(), "C1", ts, "lo"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(t.Context(), "C1", ts, "x", nil); err == nil || !strings.Contains(err.Error(), "cant_update_message") {
		t.Fatalf("Update of a message still streaming = %v; want cant_update_message", err)
	}
	blocks := []byte(`[{"type":"section","text":{"type":"mrkdwn","text":"Hello!"}}]`)
	if err := c.StopStream(t.Context(), "C1", ts, "!", blocks, "ob-1"); err != nil {
		t.Fatal(err)
	}
	m := reply()
	if m.Text != "Hello!" || m.Streaming || len(m.Blocks) == 0 || m.Metadata == nil || m.Metadata.EventType != "courier_message" || m.Metadata.EventPayload["outbox_id"] != "ob-1" {
		t.Fatalf("after StopStream: %+v; want the whole text, blocks, and the outbox metadata", m)
	}
	// Once stopped the message can be edited.
	if err := c.Update(t.Context(), "C1", ts, "final", blocks); err != nil {
		t.Fatal(err)
	}
	if m := reply(); m.Text != "final" || m.Edited == nil {
		t.Fatalf("after Update: %+v; want the new text, edited", m)
	}
}

func TestStartStreamNeedsARecipient(t *testing.T) {
	c, _, _, root := updateClient(t)
	if _, err := c.StartStream(t.Context(), "C1", root, "", "x"); err == nil || !strings.Contains(err.Error(), "missing_recipient_user_id") {
		t.Fatalf("StartStream with no recipient = %v; want missing_recipient_user_id", err)
	}
}

func TestTeamIDIsAskedOnce(t *testing.T) {
	c, _, calls, _ := updateClient(t)
	for range 2 {
		if id, err := c.TeamID(t.Context()); err != nil || id != "TFAKE" {
			t.Fatalf("TeamID = %q, %v; want TFAKE", id, err)
		}
	}
	if n := calls.count("/auth.test"); n != 1 {
		t.Fatalf("%d auth.test calls for two TeamIDs; want 1", n)
	}
}

// TestStreamErrorsNameNoToken makes every request fail with an error that quotes both tokens;
// no method may return them.
func TestStreamErrorsNameNoToken(t *testing.T) {
	const bot, app = "canary-bot-token", "canary-app-token"
	fake, calls := newFake(t), &counter{n: map[string]int{}}
	cfg := clientConfig(fake)
	cfg.BotToken, cfg.AppToken, cfg.HTTPClient = secret.New(bot), secret.New(app), &http.Client{Transport: calls}
	c := slack.New(cfg)
	calls.err = fmt.Errorf("proxy refused %s and %s", bot, app)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"Update":       func() error { return c.Update(ctx, "C1", "1.1", "x", nil) },
		"StartStream":  func() error { _, err := c.StartStream(ctx, "C1", "1.1", "U1", "x"); return err },
		"AppendStream": func() error { return c.AppendStream(ctx, "C1", "1.1", "x") },
		"StopStream":   func() error { return c.StopStream(ctx, "C1", "1.1", "x", nil, "ob") },
		"TeamID":       func() error { _, err := c.TeamID(ctx); return err },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s succeeded through a failing transport", name)
			continue
		}
		if strings.Contains(err.Error(), bot) || strings.Contains(err.Error(), app) {
			t.Errorf("%s error holds a token: %v", name, err)
		}
	}
}
