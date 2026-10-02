package slack_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	slackgo "github.com/slack-go/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const intakeConn = "courier-system/slack-main"

// submitter records what the session manager is asked to submit.
type submitter struct {
	mu  sync.Mutex
	got []session.Inbound
}

func (s *submitter) Submit(_ context.Context, in session.Inbound) (session.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
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

// startIntake runs the Slack adapter against a fake Slack with the neutral Intake as its sink, over a config that
// binds C1 to sre/sre-alerts: what a message event becomes when it goes through the adapter and the trigger table.
func startIntake(t *testing.T) (*fakeslack.Server, *submitter) {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	allow := []string{"sre"}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("courier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: allow}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("courier-system", "slack-main"), Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: allow}}},
		Bindings: []v1alpha1.Binding{{ObjectMeta: meta("sre", "sre-alerts"), Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "kagent"}},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"}, Channel: "C1"}}}},
	}, allKeys{})
	if err != nil {
		t.Fatal(err)
	}
	sub := &submitter{}
	intake := chat.NewIntake(chat.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"},
		Snapshot:   func() *config.Snapshot { return snap }, Submitter: sub, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	fake := newFake(t)
	platform := slack.NewPlatform(slack.New(clientConfig(fake)), intakeConn)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- platform.Run(ctx, intake) }()
	t.Cleanup(func() { cancel(); <-done })
	waitConnected(t, fake)
	return fake, sub
}

// TestPairCollapses sends the app_mention and the message event of one mention with
// different event IDs and event_ts, as Slack does: both are the one turn, whose ID is built
// from the message's ts.
func TestPairCollapses(t *testing.T) {
	fake, sub := startIntake(t)
	mention := message(map[string]any{"type": "app_mention", "ts": "7.7", "event_ts": "8.1", "text": botMark + " hi"})
	msg := message(map[string]any{"ts": "7.7", "event_ts": "8.2", "text": botMark + " hi"})
	waitAck(t, fake, send(t, fake, mention), send(t, fake, msg))
	want := []session.Inbound{{Key: session.Key{Connection: intakeConn, Channel: "C1", Thread: "7.7"}, Binding: "sre/sre-alerts",
		MessageID: "slack:" + intakeConn + ":C1:7.7", MessageRef: "7.7", UserID: "U1", Text: botMark + " hi", Create: true}}
	if got := sub.submitted(); !slices.Equal(got, want) {
		t.Fatalf("submitted\n%+v\nwant\n%+v", got, want)
	}
}

// TestNoLoop runs a reply cycle: a mention, courier's answer in the thread, and a user's
// reply. Slack sends courier's own post back as an event, and it must start no turn.
func TestNoLoop(t *testing.T) {
	fake, sub := startIntake(t)
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
	want := []string{"slack:" + intakeConn + ":C1:" + root.TS, "slack:" + intakeConn + ":C1:" + reply.TS}
	if !slices.Equal(ids, want) {
		t.Fatalf("submitted turns %v; want the mention and the reply, not courier's own post %s", ids, answer)
	}
}
