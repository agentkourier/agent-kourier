package slack_test

import (
	"encoding/json"
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
	ts, err := c.StartStream(t.Context(), "C1", root, "U1", "Hel", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if m := reply(); m.TS != ts || m.ThreadTS != root || m.Text != "Hel" || !m.Streaming {
		t.Fatalf("after StartStream: %+v; want a streaming reply in the thread with the first text", m)
	}
	if err := c.AppendStream(t.Context(), "C1", ts, "lo", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(t.Context(), "C1", ts, slack.Content{Text: "x"}); err == nil || !strings.Contains(err.Error(), "cant_update_message") {
		t.Fatalf("Update of a message still streaming = %v; want cant_update_message", err)
	}
	blocks := []byte(`[{"type":"section","text":{"type":"mrkdwn","text":"Hello!"}}]`)
	if err := c.StopStream(t.Context(), "C1", ts, "!", blocks, "ob-1", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	m := reply()
	if m.Text != "Hello!" || m.Streaming || len(m.Blocks) == 0 || m.Metadata == nil || m.Metadata.EventType != "courier_message" || m.Metadata.EventPayload["outbox_id"] != "ob-1" {
		t.Fatalf("after StopStream: %+v; want the whole text, blocks, and the outbox metadata", m)
	}
	// Once stopped the message can be edited: new text and blocks replace the old, and empty
	// blocks clear them.
	second := json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"second blocks"}}]`)
	if err := c.Update(t.Context(), "C1", ts, slack.Content{Text: "final", Blocks: second}); err != nil {
		t.Fatal(err)
	}
	if m := reply(); m.Text != "final" || m.Edited == nil || !strings.Contains(string(m.Blocks), "second blocks") || strings.Contains(string(m.Blocks), "Hello!") {
		t.Fatalf("after Update: %+v; want the new text, edited, and the new blocks", m)
	}
	if err := c.Update(t.Context(), "C1", ts, slack.Content{Text: "plain", Blocks: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if m := reply(); m.Text != "plain" || len(m.Blocks) != 0 {
		t.Fatalf("after Update with []: %+v; want the blocks cleared", m)
	}
}

func TestStopStreamNeedsAnOutboxID(t *testing.T) {
	c, _, calls, root := updateClient(t)
	ts, err := c.StartStream(t.Context(), "C1", root, "U1", "x", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StopStream(t.Context(), "C1", ts, "", nil, "", slack.ChunksMode); err == nil || !strings.Contains(err.Error(), "an outbox ID is required") {
		t.Fatalf("StopStream with no outbox ID = %v; want it refused, as Post does", err)
	}
	if n := calls.count("/chat.stopStream"); n != 0 {
		t.Fatalf("%d chat.stopStream calls for a refused stop; want none", n)
	}
}

func TestTeamIDNeedsATeam(t *testing.T) {
	c, _, calls, _ := updateClient(t)
	calls.status, calls.body = http.StatusOK, `{"ok":true,"user_id":"UFAKEBOT"}` // auth.test with no team_id
	if id, err := c.TeamID(t.Context()); err == nil || !strings.Contains(err.Error(), "no team ID") {
		t.Fatalf("TeamID = %q, %v; want an error for an answer with no team", id, err)
	}
}

func TestStartStreamNeedsARecipient(t *testing.T) {
	c, _, _, root := updateClient(t)
	if _, err := c.StartStream(t.Context(), "C1", root, "", "x", slack.ChunksMode); err == nil || !strings.Contains(err.Error(), "missing_recipient_user_id") {
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
// no method may return them. TeamID is asked first, on a client that has not cached the team,
// and the others on one that has, so that each fails in its own call.
func TestStreamErrorsNameNoToken(t *testing.T) {
	const bot, app = "xoxb-canary-bot-token", "xapp-canary-app-token" // with the prefixes the fake accepts
	fake := newFake(t)
	client := func(warm bool) *slack.Client {
		calls := &counter{n: map[string]int{}}
		cfg := clientConfig(fake)
		cfg.BotToken, cfg.AppToken, cfg.HTTPClient = secret.New(bot), secret.New(app), &http.Client{Transport: calls}
		c := slack.New(cfg)
		if warm {
			if _, err := c.TeamID(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		calls.err = fmt.Errorf("proxy refused %s and %s", bot, app)
		return c
	}
	ctx := t.Context()
	cold, warm := client(false), client(true)
	for name, call := range map[string]func() error{
		"TeamID":       func() error { _, err := cold.TeamID(ctx); return err },
		"Update":       func() error { return warm.Update(ctx, "C1", "1.1", slack.Content{Text: "x"}) },
		"StartStream":  func() error { _, err := warm.StartStream(ctx, "C1", "1.1", "U1", "x", slack.ChunksMode); return err },
		"AppendStream": func() error { return warm.AppendStream(ctx, "C1", "1.1", "x", slack.ChunksMode) },
		"AppendStep":   func() error { return warm.AppendStep(ctx, "C1", "1.1", slack.TaskUpdate{ID: "s1", Title: "x"}) },
		"StopStream":   func() error { return warm.StopStream(ctx, "C1", "1.1", "x", nil, "ob", slack.ChunksMode) },
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

// TestRequestsAreInert: agent-written text must not become a live broadcast or a link Slack's
// servers fetch (spec 8.4). Every post, update and stream request turns off unfurling, leaves
// link_names unset, and does not ask for parse=full.
// UNCONFIRMED(WP-04): that the streaming methods accept, or at least ignore, the unfurl options.
func TestRequestsAreInert(t *testing.T) {
	c, _, calls, root := updateClient(t)
	ctx := t.Context()
	if _, err := c.Post(ctx, "C1", root, slack.Content{Text: "@here https://evil.example/?d=x"}, "ob-1"); err != nil {
		t.Fatal(err)
	}
	ts, err := c.StartStream(ctx, "C1", root, "U1", "@here", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AppendStream(ctx, "C1", ts, "https://evil.example/", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	if err := c.StopStream(ctx, "C1", ts, "", nil, "ob-2", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, "C1", ts, slack.Content{Text: "@channel"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/chat.postMessage", "/chat.startStream", "/chat.appendStream", "/chat.stopStream", "/chat.update"} {
		forms := calls.sent(path)
		if len(forms) == 0 {
			t.Errorf("no %s request was made", path)
		}
		for _, f := range forms {
			if f.Get("unfurl_links") != "false" || f.Get("unfurl_media") != "false" || f.Has("link_names") || f.Get("parse") == "full" {
				t.Errorf("%s sent unfurl_links=%q unfurl_media=%q link_names=%q parse=%q; want both unfurls false, no link_names, no parse=full",
					path, f.Get("unfurl_links"), f.Get("unfurl_media"), f.Get("link_names"), f.Get("parse"))
			}
		}
	}
}
