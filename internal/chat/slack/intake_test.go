package slack_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	slackgo "github.com/slack-go/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const conn = "courier-system/slack-main"

// The session manager is what Submit goes to.
var _ slack.Submitter = (*session.Manager)(nil)

// submitter records what the session manager is asked to submit. fn, if set, decides the
// answer; by default a turn is Accepted.
type submitter struct {
	mu  sync.Mutex
	got []session.Inbound
	fn  func(session.Inbound) (session.Result, error)
}

func (s *submitter) Submit(_ context.Context, in session.Inbound) (session.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
	if s.fn != nil {
		return s.fn(in)
	}
	return session.Result{Disposition: session.Accepted}, nil
}

func (s *submitter) submitted() []session.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// allKeys is a SecretResolver whose Secrets have every key.
type allKeys struct{}

func (allKeys) Keys(context.Context, string, string) ([]string, error) {
	return []string{"botToken", "appToken", "token"}, nil
}

// newIntake returns an Intake for the connection, over a config that binds C1 to sre/sre-alerts
// and the private channel G1 to payments/pay.
func newIntake(t *testing.T) (*slack.Intake, *submitter) {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	bind := func(ns, name, channel string) v1alpha1.Binding {
		return v1alpha1.Binding{ObjectMeta: meta(ns, name), Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "kagent"}},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"}, Channel: channel}}}
	}
	allow := []string{"sre", "payments"}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("courier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{AllowedNamespaces: allow}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("courier-system", "slack-main"), Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: allow}}},
		Bindings:        []v1alpha1.Binding{bind("sre", "sre-alerts", "C1"), bind("payments", "pay", "G1")},
	}, allKeys{})
	if err != nil {
		t.Fatal(err)
	}
	sub := &submitter{}
	return slack.NewIntake(slack.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"},
		Snapshot:   func() *config.Snapshot { return snap }, Submitter: sub,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}), sub
}

// TestTriggers checks spec 8.2's table: what each message is submitted as, if at all.
func TestTriggers(t *testing.T) {
	const text = "<@UFAKEBOT> look"
	tests := []struct {
		name    string
		m       slack.Message
		submit  bool
		create  bool
		binding string
		thread  string
	}{
		{"mention outside a thread starts one", slack.Message{Channel: "C1", TS: "5.5", Mentioned: true}, true, true, "sre/sre-alerts", "5.5"},
		{"mention in a thread, owned or not", slack.Message{Channel: "C1", TS: "5.5", ThreadTS: "4.4", Mentioned: true}, true, true, "sre/sre-alerts", "4.4"},
		{"reply in a thread", slack.Message{Channel: "C1", TS: "5.5", ThreadTS: "4.4"}, true, false, "", "4.4"},
		{"mention in a private channel", slack.Message{Channel: "G1", TS: "5.5", Mentioned: true}, true, true, "payments/pay", "5.5"},
		{"mention in an unbound channel's thread", slack.Message{Channel: "C9", TS: "5.5", ThreadTS: "4.4", Mentioned: true}, true, false, "", "4.4"},
		{"mention in an unbound channel", slack.Message{Channel: "C9", TS: "5.5", Mentioned: true}, false, false, "", ""},
		{"message outside any thread", slack.Message{Channel: "C1", TS: "5.5"}, false, false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntake(t)
			tt.m.User, tt.m.Text = "U1", text
			if err := in.Message(t.Context(), tt.m); err != nil {
				t.Fatal(err)
			}
			var want []session.Inbound
			if tt.submit {
				want = []session.Inbound{{Key: session.Key{Connection: conn, Channel: tt.m.Channel, ThreadTS: tt.thread}, Binding: tt.binding,
					MessageID: session.SlackMessageID(conn, tt.m.Channel, "5.5"), MessageTS: "5.5", UserID: "U1", Text: text, Create: tt.create}}
			}
			if got := sub.submitted(); !slices.Equal(got, want) {
				t.Fatalf("submitted\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestSubmitOutcomes delivers one message twice to see what Submit's answer does: whether
// the message is acknowledged (a nil error) and whether it is remembered. A message is
// remembered only once a turn is queued, so a redelivery is not submitted again; any other
// answer leaves the next event of the message, such as the app_mention that follows a
// message, free to try.
func TestSubmitOutcomes(t *testing.T) {
	disposition := func(d session.Disposition) (session.Result, error) { return session.Result{Disposition: d}, nil }
	tests := []struct {
		name    string
		answer  func() (session.Result, error)
		wantErr bool
		again   bool // the second delivery is submitted
	}{
		{"created", func() (session.Result, error) { return disposition(session.Created) }, false, false},
		{"accepted", func() (session.Result, error) { return disposition(session.Accepted) }, false, false},
		{"not owned", func() (session.Result, error) { return disposition(session.NotOwned) }, false, true},
		{"expired", func() (session.Result, error) { return disposition(session.Expired) }, false, true},
		{"binding gone", func() (session.Result, error) { return disposition(session.BindingGone) }, false, true},
		{"invalid", func() (session.Result, error) { return session.Result{}, fmt.Errorf("x: %w", store.ErrInvalid) }, false, true},
		{"store failure", func() (session.Result, error) { return session.Result{}, errors.New("disk full") }, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntake(t)
			sub.fn = func(session.Inbound) (session.Result, error) { return tt.answer() }
			m := slack.Message{Channel: "C1", TS: "5.5", ThreadTS: "4.4", User: "U1", Text: "hi", Mentioned: true}
			if err := in.Message(t.Context(), m); (err != nil) != tt.wantErr {
				t.Fatalf("Message = %v; want error: %v", err, tt.wantErr)
			}
			_ = in.Message(t.Context(), m)
			if n := len(sub.submitted()); (n == 2) != tt.again {
				t.Fatalf("Submit called %d times for two deliveries; want a second call: %v", n, tt.again)
			}
		})
	}
}

// TestDedupKey checks that the two events of one message are one submit however they
// differ (an app_mention has no channel_type), and that the same ts in another channel is
// another message.
func TestDedupKey(t *testing.T) {
	in, sub := newIntake(t)
	m := slack.Message{Channel: "C1", TS: "5.5", User: "U1", Text: "hi", Mentioned: true}
	other := m
	other.Channel = "G1"
	withType := m
	withType.ChannelType = "channel"
	for _, msg := range []slack.Message{m, withType, other} {
		if err := in.Message(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if got := sub.submitted(); len(got) != 2 || got[0].Key.Channel != "C1" || got[1].Key.Channel != "G1" {
		t.Fatalf("submitted %+v; want one turn in C1 and one in G1", got)
	}
}

// TestPairCollapses sends the app_mention and the message event of one mention with
// different event IDs and event_ts, as Slack does: both are the one turn, whose ID is built
// from the message's ts.
func TestPairCollapses(t *testing.T) {
	in, sub := newIntake(t)
	fake := start(t, in)
	mention := message(map[string]any{"type": "app_mention", "ts": "7.7", "event_ts": "8.1", "text": botMark + " hi"})
	msg := message(map[string]any{"ts": "7.7", "event_ts": "8.2", "text": botMark + " hi"})
	waitAck(t, fake, send(t, fake, mention), send(t, fake, msg))
	want := []session.Inbound{{Key: session.Key{Connection: conn, Channel: "C1", ThreadTS: "7.7"}, Binding: "sre/sre-alerts",
		MessageID: "slack:" + conn + ":C1:7.7", MessageTS: "7.7", UserID: "U1", Text: botMark + " hi", Create: true}}
	if got := sub.submitted(); !slices.Equal(got, want) {
		t.Fatalf("submitted\n%+v\nwant\n%+v", got, want)
	}
}

// TestNoLoop runs a reply cycle: a mention, courier's answer in the thread, and a user's
// reply. Slack sends courier's own post back as an event, and it must start no turn.
func TestNoLoop(t *testing.T) {
	in, sub := newIntake(t)
	fake := start(t, in)
	root, err := fake.Mention("C1", "U1", botMark+" why?")
	if err != nil {
		t.Fatal(err)
	}
	api := slackgo.New(botToken, slackgo.OptionAPIURL(fake.URL()+"/"))
	_, answer, err := api.PostMessage("C1", slackgo.MsgOptionText("because", false), slackgo.MsgOptionTS(root.TS))
	if err != nil {
		t.Fatal(err)
	}
	own := fake.OwnEventEnvelopes("C1", answer)
	if len(own) != 1 {
		t.Fatalf("the fake sent %d events for courier's post; want 1", len(own))
	}
	reply, _ := fake.Reply("C1", root.TS, "U2", "and then?")
	waitAck(t, fake, slices.Concat(root.Envelopes, own, reply.Envelopes)...)
	var ids []string
	for _, s := range sub.submitted() {
		ids = append(ids, s.MessageID)
	}
	want := []string{session.SlackMessageID(conn, "C1", root.TS), session.SlackMessageID(conn, "C1", reply.TS)}
	if !slices.Equal(ids, want) {
		t.Fatalf("submitted turns %v; want the mention and the reply, not courier's own post %s", ids, answer)
	}
}

// TestIntakePassesInteractionsOn: a press goes to the configured handler, and the handler's
// error is the Intake's, so the event is left unacknowledged.
func TestIntakePassesInteractionsOn(t *testing.T) {
	var got []slack.Interaction
	fail := errors.New("busy")
	in := slack.NewIntake(slack.IntakeConfig{Interactions: func(_ context.Context, i slack.Interaction) error {
		got = append(got, i)
		return fail
	}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	press := slack.Interaction{User: "U1", Channel: "C1", MessageTS: "5.5", ActionID: "submit"}
	if err := in.Interaction(t.Context(), press); !errors.Is(err, fail) || len(got) != 1 || got[0].ActionID != "submit" {
		t.Fatalf("Interaction = %v, handler saw %+v; want its error, and the press", err, got)
	}
	// With no handler the press is dropped, and acknowledged.
	if err := slack.NewIntake(slack.IntakeConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Interaction(t.Context(), press); err != nil {
		t.Fatalf("Interaction with no handler = %v; want nil", err)
	}
}
