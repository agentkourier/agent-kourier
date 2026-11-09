package slack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// A failed attempt to connect ends Run: slack-go would retry it without end, with a pause that grows to 5 minutes, and
// the broker's policy is the one that paces a reconnect. The watch records the error for Run to return, whether or not the
// connection had been up, takes the connection down, and logs nothing: RunPlatform logs the error it is returned.
func TestAConnectionErrorEndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prior  []socketmode.Event
		states []bool
	}{
		{"after the connection was up", []socketmode.Event{hello(), connecting(1, 1)}, []bool{true, false}},
		{"before it ever was", []socketmode.Event{connecting(0, 1)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newWatched(t)
			for _, ev := range tc.prior {
				k.w.event(t.Context(), ev)
			}
			if k.w.lost != nil {
				t.Fatalf("lost = %v before any error", k.w.lost)
			}

			k.w.event(t.Context(), connectionError(errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), 1))

			if k.w.lost == nil || !strings.Contains(k.w.lost.Error(), "connection attempt failed") || !strings.Contains(k.w.lost.Error(), "connection refused") {
				t.Errorf("lost = %v; want the failed attempt and its cause", k.w.lost)
			}
			if k.w.up || fmt.Sprint(k.states) != fmt.Sprint(tc.states) {
				t.Errorf("up = %v and OnConnection was told %v; want the connection down, having been told %v", k.w.up, k.states, tc.states)
			}
			if got := k.lines("level=WARN"); len(got) != 0 {
				t.Errorf("the watch logged %q; want nothing, since the error it hands to Run is logged by whoever retries", got)
			}
		})
	}
}

// What Classify reads is kept in the error Run returns: the code Slack refused with (a token it will never accept is
// fatal, and slack-go itself retries token_expired without end) and the wait that Slack named.
func TestTheErrorOfAConnectionAttemptKeepsWhatClassifyReads(t *testing.T) {
	k := newWatched(t)
	k.w.event(t.Context(), connectionError(fmt.Errorf("open: %w", slackgo.SlackErrorResponse{Err: "token_expired"}), 1))
	var resp slackgo.SlackErrorResponse
	if !errors.As(k.w.lost, &resp) || resp.Err != "token_expired" {
		t.Errorf("lost = %v; want Slack's code token_expired to be kept", k.w.lost)
	}

	k = newWatched(t)
	k.w.event(t.Context(), connectionError(&slackgo.RateLimitedError{RetryAfter: 42 * time.Second}, 1))
	var limited *slackgo.RateLimitedError
	if !errors.As(k.w.lost, &limited) || limited.RetryAfter != 42*time.Second {
		t.Errorf("lost = %v; want the rate limit and its Retry-After to be kept", k.w.lost)
	}
}

// A refresh is no failure. slack-go closes the socket itself and then reads from it, which it reports as a read
// error on a closed network connection: that is DEBUG, while any other read error is a WARN. Both are checked on
// the original error, which the scrubbed one only describes.
func TestAReadOnASocketSlackGoClosedIsNotAWarning(t *testing.T) {
	k := newWatched(t)
	incoming := func(err error) socketmode.Event {
		return socketmode.Event{Type: socketmode.EventTypeIncomingError, Data: &slackgo.IncomingEventError{ErrorObj: err}}
	}
	k.w.event(t.Context(), incoming(fmt.Errorf("read tcp 10.0.0.1:1->52.0.0.1:443: %w", net.ErrClosed)))
	if got := k.lines("level=WARN"); len(got) != 0 {
		t.Errorf("a read on a closed socket logged %q; want no WARN", got)
	}
	if got := k.lines("level=DEBUG"); len(got) != 1 {
		t.Errorf("a read on a closed socket logged %q at DEBUG; want one line", got)
	}
	k.w.event(t.Context(), incoming(errors.New("connection reset by peer")))
	if got := k.lines("level=WARN"); len(got) != 1 {
		t.Errorf("another read error logged %q; want one WARN", got)
	}
}

// Once Run's context has ended, slack-go still delivers the events it was sending (the attempt that the end
// cancelled, the read of a socket it closed), and none of them is news: no WARN and no INFO, whatever the order.
func TestNothingIsLoggedOnceTheContextHasEnded(t *testing.T) {
	k := newWatched(t)
	k.w.event(t.Context(), hello())
	logged := len(k.lines("level="))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for _, ev := range []socketmode.Event{
		connecting(1, 1),
		connectionError(fmt.Errorf("post: %w", context.Canceled), 1),
		connectionError(errors.New("an error that is not the cancellation"), 1), // slack-go sends it after cancel too
		{Type: socketmode.EventTypeIncomingError, Data: &slackgo.IncomingEventError{ErrorObj: net.ErrClosed}},
		{Type: socketmode.EventTypeIncomingError, Data: &slackgo.IncomingEventError{ErrorObj: errors.New("use of closed network connection")}},
	} {
		k.w.event(ctx, ev)
	}
	if got := k.lines("level="); len(got) != logged {
		t.Errorf("events after the context ended logged %q; want nothing more", got[logged:])
	}
	if k.w.lost != nil {
		t.Errorf("lost = %v once the context had ended; the end of Run is not a lost connection", k.w.lost)
	}
	if fmt.Sprint(k.states) != fmt.Sprint([]bool{true}) {
		t.Errorf("OnConnection was told %v; want only up: closing the watch, which Run defers, takes it down", k.states)
	}
	k.w.close()
	if fmt.Sprint(k.states) != fmt.Sprint([]bool{true, false}) {
		t.Errorf("after close OnConnection was told %v; want up, down", k.states)
	}
}

// A connection error that is the cancellation of its own attempt is not logged even if the context is still live
// when it is read: it says nothing of Slack.
func TestACancelledAttemptIsNotAFailure(t *testing.T) {
	k := newWatched(t)
	k.w.event(t.Context(), connectionError(fmt.Errorf("Post: %w", context.Canceled), 1))
	if got := k.lines("level=WARN"); len(got) != 0 || k.w.lost != nil {
		t.Errorf("a cancelled attempt logged %q and ended the run with %v; want neither", got, k.w.lost)
	}
}

// Connecting alone takes the connection down, with no error between: that is what a refresh looks like, because
// slack-go closes the socket and reconnects, and sends nothing for the loss. It says reconnecting, at INFO, once.
func TestConnectingAloneTakesTheConnectionDown(t *testing.T) {
	k := newWatched(t)
	for _, ev := range []socketmode.Event{hello(), connecting(1, 1), hello()} {
		k.w.event(t.Context(), ev)
	}
	if want := []bool{true, false, true}; fmt.Sprint(k.states) != fmt.Sprint(want) {
		t.Errorf("OnConnection was told %v; want %v", k.states, want)
	}
	if got := k.lines("level=INFO msg=\"slack: socket mode reconnecting\""); len(got) != 1 {
		t.Errorf("a refresh logged %q; want one INFO reconnecting line", got)
	}
	if got := k.lines("level=WARN"); len(got) != 0 {
		t.Errorf("a refresh logged %q; want no WARN", got)
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
	if got := k.lines("socket mode reconnecting"); len(got) != 1 || !strings.Contains(got[0], "level=INFO") {
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
	const plainTicket = "TICKET-77c1e" // a socket URL without TLS is a URL too: a proxy, or a test's fake
	url := `wss://wss-primary.slack.com/link/?ticket=` + ticket + `&app_id=A123`
	plain := `ws://127.0.0.1:8443/link?ticket=` + plainTicket
	leaky := func(what string) error {
		return fmt.Errorf("%s: Get %q then %q: xapp-fake-app-token and xoxb-fake-bot-token: %w", what, url, plain, errors.New("EOF"))
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
	for _, bad := range []string{ticket, plainTicket, "wss://", "ws://", "xapp-fake-app-token", "xoxb-fake-bot-token", "USER TEXT"} {
		if strings.Contains(logs, bad) {
			t.Errorf("the log holds %q:\n%s", bad, logs)
		}
	}
	if got := len(k.lines("level=WARN")); got != 3 {
		t.Errorf("%d WARN lines; want one each for the read error, the bad message and the write error:\n%s", got, logs)
	}
	// The connection error is not logged here but returned from Run, and RunPlatform logs it: it holds no secret either.
	for _, bad := range []string{ticket, plainTicket, "wss://", "ws://", "xapp-fake-app-token", "xoxb-fake-bot-token"} {
		if k.w.lost == nil || strings.Contains(k.w.lost.Error(), bad) {
			t.Errorf("the error that ends Run is %v; want one without %q", k.w.lost, bad)
		}
	}
	if got := k.lines("env-2"); len(got) != 1 || !strings.Contains(got[0], "level=DEBUG") || !strings.Contains(got[0], "events_api") {
		t.Errorf("an envelope logged %q; want its type and ID at DEBUG", got)
	}
}
