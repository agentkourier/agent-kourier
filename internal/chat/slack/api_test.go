package slack_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// A client behind a wrapper that embeds API (a decorator over the client, or a test double over it) keeps the busy mark,
// thread reads and permalinks: the Platform asks the API for them, and API has them, so no wrapper can hide one.
func TestAWrappedClientKeepsItsCapabilities(t *testing.T) {
	t.Run("busy mark", func(t *testing.T) {
		rt := &statusSlack{}
		c := slack.New(slack.Config{BotToken: secret.New("xoxb-fake-token"), AppToken: secret.New("xapp-fake-token"), APIURL: "http://slack.test/",
			HTTPClient: &http.Client{Transport: rt}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		p := slack.NewPlatform(struct{ slack.API }{c}, "ns/conn")
		if err := p.MarkBusy(t.Context(), busyThread, true); err != nil || len(rt.calls()) != 1 {
			t.Errorf("MarkBusy = %v with %d calls; want the status set by one call", err, len(rt.calls()))
		}
	})
	t.Run("thread reads", func(t *testing.T) {
		th := newThread(t)
		r := th.reply(2)
		p := slack.NewPlatform(struct{ slack.API }{th.c}, threadConn)
		got, _, err := p.ThreadBetween(t.Context(), chat.Target{Conn: threadConn, Channel: "C1", Thread: th.root}, "", r[1], 10)
		if err != nil || len(got) != 1 {
			t.Errorf("ThreadBetween = %+v, %v; want the first reply", got, err)
		}
	})
	t.Run("permalinks", func(t *testing.T) {
		fake := newFake(t, fakeslack.WithTeam("T0EXAMPLE1", "example"))
		client := slack.New(clientConfig(fake))
		p := slack.NewPlatform(struct{ slack.API }{client}, intakeConn)
		run(t, client, &sink{})
		waitConnected(t, fake)
		if got, ok := p.Permalink(ref("C0123456789", "1759560000.123456")); !ok || got == "" {
			t.Errorf("Permalink = %q, %v; want the message's link", got, ok)
		}
	})
}
