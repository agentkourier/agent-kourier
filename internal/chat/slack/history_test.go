package slack_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// countingTransport counts the calls to conversations.history that pass through it.
type countingTransport struct{ history atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/conversations.history") {
		c.history.Add(1)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// History reads the whole channel through as many pages as it takes and returns it oldest first, each message
// decoded as Run decodes a live one: a bot's with its attachment flattened, a person's with its mention, courier's own
// post marked, and every Ref naming the connection.
func TestHistoryPagesThroughTheChannelOldestFirst(t *testing.T) {
	fake := newFake(t)
	transport := &countingTransport{}
	cfg := clientConfig(fake)
	cfg.PageLimit = 2
	cfg.HTTPClient = &http.Client{Transport: transport}
	c := slack.New(cfg)
	run(t, c, &sink{})
	waitConnected(t, fake)

	var posted []string
	post := func(p fakeslack.WebhookPost) {
		t.Helper()
		got, err := fake.Webhook(p)
		if err != nil {
			t.Fatal(err)
		}
		posted = append(posted, got.TS)
	}
	for _, title := range []string{"[kind] FIRING (1) - A", "[kind] RESOLVED - A"} {
		p := alert()
		p.Attachments[0].Title = title
		post(p)
	}
	mention, err := fake.Mention("C1", "U1", botMark+" what is this?")
	if err != nil {
		t.Fatal(err)
	}
	posted = append(posted, mention.TS)
	own := alert()
	own.BotID = "BFAKEBOT"
	post(own)
	post(alert())
	post(alert())

	got, err := c.History(t.Context(), "courier-system/slack-main", "C1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if n := transport.history.Load(); n < 3 {
		t.Errorf("History made %d calls to conversations.history; want one per page of 2 messages, at least 3", n)
	}
	if len(got) != len(posted) {
		t.Fatalf("History returned %d messages, want %d", len(got), len(posted))
	}
	for i, h := range got {
		if h.Message.At.ID != posted[i] {
			t.Errorf("message %d is %s; want %s: the history is oldest first", i, h.Message.At.ID, posted[i])
		}
		if want := (chat.Ref{Platform: "slack", Conn: "courier-system/slack-main", Channel: "C1", ID: posted[i]}); h.Message.At != want {
			t.Errorf("message %d is at %+v; want %+v", i, h.Message.At, want)
		}
	}
	a := alert().Attachments[0]
	if b := got[0].Message.Bot; b == nil || b.BotID != webhookBot || b.Title != "[kind] FIRING (1) - A" || b.TitleLink != a.TitleLink || b.Fallback != a.Fallback ||
		b.Footer != a.Footer || b.AttachmentText != a.Text || got[0].Own || got[0].Ignored {
		t.Errorf("the first message = %+v; want the webhook's alert, flattened, neither own nor ignored", got[0])
	}
	if m := got[2].Message; m.Bot != nil || m.User != "U1" || m.Text != botMark+" what is this?" || !m.Mentioned {
		t.Errorf("the person's message = %+v; want U1's mention", m)
	}
	for i, wantOwn := range []bool{false, false, false, true, false, false} {
		if got[i].Own != wantOwn {
			t.Errorf("message %d: Own = %t; want %t", i, got[i].Own, wantOwn)
		}
	}
}

// Only messages after since are read, and an empty channel is no error.
func TestHistoryReadsFromSince(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	run(t, c, &sink{})
	waitConnected(t, fake)
	if _, err := fake.Webhook(alert()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		since time.Time
		want  int
	}{
		{"from the start", time.Time{}, 1},
		{"from an hour ago", time.Now().Add(-time.Hour), 1},
		{"from an hour ahead", time.Now().Add(time.Hour), 0},
	} {
		got, err := c.History(t.Context(), "courier-system/slack-main", "C1", tc.since)
		if err != nil || len(got) != tc.want {
			t.Errorf("%s: History = %d messages, %v; want %d", tc.name, len(got), err, tc.want)
		}
	}
}

// A failure names the call and Slack's code, and never the bot token.
func TestHistoryErrorsDoNotCarryTheToken(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	_, err := c.History(t.Context(), "courier-system/slack-main", "", time.Time{}) // no channel: channel_not_found
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("History = %v; want Slack's channel_not_found", err)
	}
	if strings.Contains(err.Error(), botToken) {
		t.Errorf("the error carries the bot token: %v", err)
	}
}
