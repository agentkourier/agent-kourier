package slack

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// watched is a Client over a clock the test moves and a debug log it can read, with the states
// that OnConnection was told.
type watched struct {
	c      *Client
	w      *watch
	logs   *bytes.Buffer
	now    time.Time
	states []bool
}

func newWatched(t *testing.T) *watched {
	t.Helper()
	k := &watched{logs: &bytes.Buffer{}, now: time.Unix(1_700_000_000, 0)}
	k.c = New(Config{
		BotToken: secret.New("xoxb-fake-bot-token"), AppToken: secret.New("xapp-fake-app-token"),
		Logger:       slog.New(slog.NewTextHandler(k.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:          func() time.Time { return k.now },
		OnConnection: func(up bool) { k.states = append(k.states, up) },
	})
	k.w = &watch{c: k.c}
	return k
}

// lines are the log lines holding sub.
func (k *watched) lines(sub string) []string {
	var out []string
	for _, l := range strings.Split(k.logs.String(), "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func connectionError(err error, attempt int) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeConnectionError, Data: &slackgo.ConnectionErrorEvent{Attempt: attempt, Backoff: time.Second, ErrorObj: err}}
}

func hello() socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeHello, Request: &socketmode.Request{Type: "hello", NumConnections: 1}}
}

func connecting(count, attempt int) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeConnecting, Data: &slackgo.ConnectingEvent{ConnectionCount: count, Attempt: attempt}}
}

// A retry loop logs its first failure at once, then a failure of the same kind at most once a minute, with a
// count of those it left out; a new kind of failure is logged at once, and so is the first one after a
// connection was made.
func TestConnectionErrorsAreCollapsed(t *testing.T) {
	k := newWatched(t)
	refused := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	for attempt := 1; attempt <= 5; attempt++ {
		k.w.event(t.Context(), connectionError(refused, attempt))
		k.now = k.now.Add(5 * time.Second)
	}
	if got := k.lines("level=WARN"); len(got) != 1 || !strings.Contains(got[0], "connection refused") {
		t.Fatalf("5 identical failures in 25 s logged %q; want one WARN line", got)
	}

	k.now = k.now.Add(time.Minute)
	k.w.event(t.Context(), connectionError(refused, 6))
	if got := k.lines("level=WARN"); len(got) != 2 || !strings.Contains(got[1], "suppressed=4") {
		t.Fatalf("the failure a minute later logged %q; want a second line that counts the 4 left out", got)
	}

	k.w.event(t.Context(), connectionError(errors.New("slack rate limit exceeded"), 7))
	if got := k.lines("level=WARN"); len(got) != 3 || !strings.Contains(got[2], "rate limit") {
		t.Fatalf("a different failure logged %q; want it at once", got)
	}

	k.w.event(t.Context(), hello())
	k.w.event(t.Context(), connectionError(errors.New("slack rate limit exceeded"), 1))
	if got := k.lines("level=WARN"); len(got) != 4 {
		t.Fatalf("a failure after a connection logged %q; want it at once, as a new outage", got)
	}
}

// The gauge follows the connection: down until Slack says hello, down again when the next attempt starts (slack-go
// sends no event for the loss itself), and never told twice of a state it is in.
func TestConnectionStateFollowsTheEvents(t *testing.T) {
	k := newWatched(t)
	ctx := t.Context()
	for _, ev := range []socketmode.Event{
		connecting(0, 1),
		{Type: socketmode.EventTypeConnected, Data: &socketmode.ConnectedEvent{}},
		hello(),
		connecting(1, 1), // the connection was lost
		connectionError(errors.New("boom"), 1),
		connecting(1, 2),
		hello(),
		{Type: socketmode.EventTypeInvalidAuth, Data: &slackgo.InvalidAuthEvent{}},
	} {
		k.w.event(ctx, ev)
	}
	if want := []bool{true, false, true, false}; fmt.Sprint(k.states) != fmt.Sprint(want) {
		t.Errorf("OnConnection was told %v; want %v", k.states, want)
	}
	if got := k.lines("socket mode connection lost"); len(got) != 1 || !strings.Contains(got[0], "level=INFO") {
		t.Errorf("a connection lost logged %q; want one INFO line", got)
	}
	if got := k.lines("msg=\"slack: socket mode connected\""); len(got) != 2 {
		t.Errorf("two hellos logged %q; want two INFO lines", got)
	}
	k.w.event(ctx, hello())
	k.w.close()
	if got := k.states[len(k.states)-2:]; fmt.Sprint(got) != fmt.Sprint([]bool{true, false}) {
		t.Errorf("after a hello and close, OnConnection was told %v; want a last state of down", k.states)
	}
}

// No lifecycle line holds a token, the URL of a socket (whose ticket is a credential for a connection) or
// anything of an envelope's payload, which is what people wrote.
func TestLifecycleLinesHoldNoTokenTicketOrPayload(t *testing.T) {
	const ticket = "TICKET-5f3a9"
	url := `wss://wss-primary.slack.com/link/?ticket=` + ticket + `&app_id=A123`
	leaky := func(what string) error {
		return fmt.Errorf("%s: Get %q: xapp-fake-app-token and xoxb-fake-bot-token: %w", what, url, errors.New("EOF"))
	}
	k := newWatched(t)
	ctx := t.Context()
	for _, ev := range []socketmode.Event{
		connectionError(leaky("open"), 1),
		{Type: socketmode.EventTypeIncomingError, Data: &slackgo.IncomingEventError{ErrorObj: leaky("read")}},
		{Type: socketmode.EventTypeErrorBadMessage, Data: &socketmode.ErrorBadMessage{
			Cause: leaky("parse"), Message: []byte(`{"payload":{"event":{"text":"USER TEXT"}}}`)}},
		{Type: socketmode.EventTypeErrorWriteFailed, Data: &socketmode.ErrorWriteFailed{
			Cause: leaky("write"), Response: &socketmode.Response{EnvelopeID: "env-1", Payload: "USER TEXT"}}},
		{Type: socketmode.EventTypeEventsAPI, Request: &socketmode.Request{Type: "events_api", EnvelopeID: "env-2", Payload: []byte(`{"text":"USER TEXT"}`)}},
	} {
		k.w.event(ctx, ev)
	}

	logs := k.logs.String()
	for _, bad := range []string{ticket, "wss://", "xapp-fake-app-token", "xoxb-fake-bot-token", "USER TEXT"} {
		if strings.Contains(logs, bad) {
			t.Errorf("the log holds %q:\n%s", bad, logs)
		}
	}
	if got := len(k.lines("level=WARN")); got != 4 {
		t.Errorf("%d WARN lines; want one each for the connection error, the read error, the bad message and the write error:\n%s", got, logs)
	}
	if got := k.lines("env-2"); len(got) != 1 || !strings.Contains(got[0], "level=DEBUG") || !strings.Contains(got[0], "events_api") {
		t.Errorf("an envelope logged %q; want its type and ID at DEBUG", got)
	}
}
