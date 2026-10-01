package slack_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Fake tokens, with the prefixes the fake Slack checks. Its bot is UFAKEBOT.
const (
	botToken = "xoxb-fake-bot-token"
	appToken = "xapp-fake-app-token"
	botMark  = "<@UFAKEBOT>"
)

// sink records the messages and interactions it is given. fn and ifn, if set, decide what
// Message and Interaction return.
type sink struct {
	mu   sync.Mutex
	got  []slack.Message
	fn   func(slack.Message) error
	ints []slack.Interaction
	ifn  func(slack.Interaction) error
}

func (s *sink) Interaction(_ context.Context, i slack.Interaction) error {
	s.mu.Lock()
	s.ints = append(s.ints, i)
	fn := s.ifn
	s.mu.Unlock()
	if fn != nil {
		return fn(i)
	}
	return nil
}

func (s *sink) interactions() []slack.Interaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ints)
}

func (s *sink) Message(_ context.Context, m slack.Message) error {
	s.mu.Lock()
	s.got = append(s.got, m)
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		return fn(m)
	}
	return nil
}

func (s *sink) messages() []slack.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// newFake starts a fake Slack and fails the test if it leaves goroutines behind.
func newFake(t *testing.T, opts ...fakeslack.Option) *fakeslack.Server {
	t.Helper()
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		goleak.VerifyNone(t, before)
	})
	fake := fakeslack.New(opts...)
	t.Cleanup(fake.Close)
	return fake
}

func clientConfig(fake *fakeslack.Server) slack.Config {
	return slack.Config{BotToken: secret.New(botToken), AppToken: secret.New(appToken), APIURL: fake.URL() + "/",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// run starts c.Run, which the end of the test cancels. wait returns Run's error.
func run(t *testing.T, c *slack.Client, sk slack.Sink) (cancel context.CancelFunc, wait func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, sk) }()
	wait = sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() { cancel(); _ = wait() })
	return cancel, wait
}

// setup is start with a recording sink.
func setup(t *testing.T) (*fakeslack.Server, *sink) {
	t.Helper()
	sk := &sink{}
	return start(t, sk), sk
}

// start runs a Client against a fake Slack, once it is connected. At the end of the test
// Run must return because its context was cancelled.
func start(t *testing.T, sk slack.Sink) *fakeslack.Server {
	t.Helper()
	fake := newFake(t)
	cancel, wait := run(t, slack.New(clientConfig(fake)), sk)
	waitConnected(t, fake)
	t.Cleanup(func() {
		if cancel(); !errors.Is(wait(), context.Canceled) {
			t.Errorf("Run after cancel = %v; want context.Canceled", wait())
		}
	})
	return fake
}

func waitConnected(t *testing.T, fake *fakeslack.Server) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); fake.Connections() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the client did not connect")
		}
	}
}

// message returns a message event; extra overrides its fields.
func message(extra map[string]any) map[string]any {
	ev := map[string]any{"type": "message", "user": "U1", "text": "hello", "ts": "1.1", "event_ts": "1.1", "channel": "C1", "channel_type": "channel"}
	maps.Copy(ev, extra)
	return ev
}

// send delivers an Events API event to the client and returns its envelope ID.
func send(t *testing.T, fake *fakeslack.Server, event map[string]any) string {
	t.Helper()
	id, err := fake.SendEnvelope("events_api", map[string]any{"type": "event_callback", "team_id": "TFAKE", "event_id": fmt.Sprint("Ev", event["type"], event["event_ts"]), "event": event})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitAck(t *testing.T, fake *fakeslack.Server, ids ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fake.WaitAck(ctx, ids...); err != nil {
		t.Fatal(err)
	}
}

func TestDeliversMentionsAndReplies(t *testing.T) {
	fake, sk := setup(t)
	fake.SetChannelType("G1", "group") // a private channel
	root, err := fake.Mention("C1", "U1", botMark+" why is it down?")
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := fake.Reply("C1", root.TS, "U2", "and now?")
	other, _ := fake.Reply("C1", root.TS, "U2", "<@U9> look")
	private, _ := fake.Mention("G1", "U1", botMark+" hi")
	waitAck(t, fake, slices.Concat(root.Envelopes, reply.Envelopes, other.Envelopes, private.Envelopes)...)
	want := []slack.Message{ // an app_mention carries no channel_type, its message event does
		{Channel: "C1", TS: root.TS, User: "U1", Text: botMark + " why is it down?", Mentioned: true},
		{Channel: "C1", TS: root.TS, User: "U1", Text: botMark + " why is it down?", Mentioned: true, ChannelType: "channel"},
		{Channel: "C1", TS: reply.TS, ThreadTS: root.TS, User: "U2", Text: "and now?", ChannelType: "channel"},
		{Channel: "C1", TS: other.TS, ThreadTS: root.TS, User: "U2", Text: "<@U9> look", ChannelType: "channel"},
		{Channel: "G1", TS: private.TS, User: "U1", Text: botMark + " hi", Mentioned: true},
		{Channel: "G1", TS: private.TS, User: "U1", Text: botMark + " hi", Mentioned: true, ChannelType: "group"},
	}
	if got := sk.messages(); !slices.Equal(got, want) {
		t.Fatalf("messages =\n%+v\nwant\n%+v", got, want)
	}
}

// TestFiltersEvents sends raw events. Each is acknowledged; only a kept one reaches the sink.
func TestFiltersEvents(t *testing.T) {
	fake, sk := setup(t)
	edit := map[string]any{"user": "U1", "text": "edited", "ts": "1.1"}
	tests := []struct {
		name string
		ev   map[string]any
		kept bool
	}{
		{"plain message", message(nil), true},
		{"thread broadcast", message(map[string]any{"subtype": "thread_broadcast", "thread_ts": "1.0"}), true},
		{"file share", message(map[string]any{"subtype": "file_share"}), true},
		{"courier's own, by user ID alone", message(map[string]any{"user": "UFAKEBOT"}), false},
		{"another bot", message(map[string]any{"user": "UOTHER", "bot_id": "BOTHER"}), false},
		{"bot_message subtype", message(map[string]any{"subtype": "bot_message"}), false},
		{"user edit", message(map[string]any{"subtype": "message_changed", "user": "", "text": "", "message": edit, "previous_message": edit}), false},
		{"deletion", message(map[string]any{"subtype": "message_deleted", "user": "", "text": "", "deleted_ts": "1.1", "previous_message": edit}), false},
		{"channel_join", message(map[string]any{"subtype": "channel_join"}), false},
		{"direct message", message(map[string]any{"channel": "D1", "channel_type": "im"}), false},
		{"empty text", message(map[string]any{"text": ""}), false},
		{"mention in an edit", message(map[string]any{"type": "app_mention", "edited": map[string]any{"user": "U1", "ts": "1.2"}}), false},
		{"mention by a bot", message(map[string]any{"type": "app_mention", "bot_id": "BOTHER"}), false},
	}
	for _, tt := range tests {
		before := len(sk.messages())
		waitAck(t, fake, send(t, fake, tt.ev))
		if got := len(sk.messages()) - before; (got == 1) != tt.kept || got > 1 {
			t.Errorf("%s: sink took %d messages; kept = %v", tt.name, got, tt.kept)
		}
	}
}

// TestOwnPostsAreIgnored has courier post, then edit, a message that mentions itself. Slack
// sends an event for each; neither may come back as a turn.
func TestOwnPostsAreIgnored(t *testing.T) {
	fake, sk := setup(t)
	api := slackgo.New(botToken, slackgo.OptionAPIURL(fake.URL()+"/"))
	_, ts, err := api.PostMessage("C1", slackgo.MsgOptionText(botMark+" an answer", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := api.UpdateMessage("C1", ts, slackgo.MsgOptionText(botMark+" an answer, edited", false)); err != nil {
		t.Fatal(err)
	}
	events := fake.OwnEventEnvelopes("C1", ts)
	if len(events) != 2 { // the post and its edit
		t.Fatalf("the fake sent %d own events; want 2", len(events))
	}
	waitAck(t, fake, events...)
	if got := sk.messages(); len(got) != 0 {
		t.Fatalf("courier's own messages reached the sink: %+v", got)
	}
}

// TestAckFollowsTheSink checks that an event stays unacknowledged while the sink holds it and
// after it fails. Events are handled in order, so the first would be acked before the next.
func TestAckFollowsTheSink(t *testing.T) {
	fake, sk := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open) // a failed check must not leave the client stuck
	sk.fn = func(m slack.Message) error {
		if m.Text != "first" {
			return nil
		}
		close(entered)
		<-release
		return errors.New("store unavailable")
	}
	first := send(t, fake, message(map[string]any{"text": "first"}))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was not called")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if err := fake.WaitAck(ctx, first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitAck while the sink holds the message = %v; want it to time out", err)
	}
	open()
	waitAck(t, fake, send(t, fake, message(nil)))
	if _, acked := fake.Ack(first); acked {
		t.Fatal("an event the sink failed on was acknowledged")
	}
}

// TestRunEnds checks that a rejected token ends Run, and that the error names no token.
func TestRunEnds(t *testing.T) {
	const canary = "canary-token-value"
	for name, tok := range map[string]func(*slack.Config){
		"bot token": func(c *slack.Config) { c.BotToken = secret.New(canary) },
		"app token": func(c *slack.Config) { c.AppToken = secret.New(canary) },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := clientConfig(newFake(t))
			tok(&cfg)
			_, wait := run(t, slack.New(cfg), &sink{})
			if err := wait(); err == nil || !strings.Contains(err.Error(), "invalid_auth") || strings.Contains(err.Error(), canary) {
				t.Fatalf("Run = %v; want invalid_auth, without the token", err)
			}
		})
	}
}

// TestRunFailsClosedWithoutABotUserID: with no user ID from auth.test, courier could not
// recognise its own posts that arrive with no bot_id, so it must not run.
func TestRunFailsClosedWithoutABotUserID(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // so that a Run that does not refuse fails the test, not hangs it
	defer cancel()
	c := slack.New(clientConfig(newFake(t, fakeslack.WithBot("", "BFAKEBOT", "AFAKEAPP"))))
	if err := c.Run(ctx, &sink{}); err == nil || !strings.Contains(err.Error(), "no bot user ID") {
		t.Fatalf("Run = %v; want it to refuse a bot with no user ID", err)
	}
}
