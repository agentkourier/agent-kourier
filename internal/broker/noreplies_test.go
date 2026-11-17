package broker

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// spySink records what reaches the renderer's sink: each turn, notice and queued notice, by its connection.
type spySink struct{ got []string }

func (s *spySink) Turn(_ context.Context, t session.TurnRef) agent.Handler {
	s.got = append(s.got, "turn "+t.Key.Connection)
	return func(agent.Event) {}
}

func (s *spySink) Notice(_ context.Context, key session.Key, _ session.Notice) {
	s.got = append(s.got, "notice "+key.Connection)
}

func (s *spySink) QueueNotice(_ context.Context, key session.Key, _ session.Notice) error {
	s.got = append(s.got, "queue "+key.Connection)
	return nil
}

// A connection whose platform has no replies (the github profile) has nothing of Agent Kourier's to post, so nothing is rendered or
// queued for it: its turns get no handler and its notices are dropped, and QueueNotice says they were written, so that no event is
// sent again for one. A connection on Slack, one of the kourier-v1 profile, which posts replies, and one the config does not have, are
// passed on as they were.
func TestAConnectionWithNoRepliesHasNothingRenderedOrQueued(t *testing.T) {
	conn := func(name string, spec v1alpha1.ChatConnectionSpec) v1alpha1.ChatConnection {
		spec.CredentialsSecretRef, spec.AllowedNamespaces = v1alpha1.SecretRef{Name: "creds"}, []string{"sre"}
		return v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: name}, Spec: spec}
	}
	set := &config.Set{ChatConnections: []v1alpha1.ChatConnection{
		conn("slack", v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack}),
		conn("github", v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWeb,
			Web: &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileGitHub, GitHub: &v1alpha1.GitHubWebSpec{Login: "pr-reviewer"}}}),
		conn("v1", v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWeb,
			Web: &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileKourierV1, URL: "https://receiver.example/kourier"}}),
	}}
	snap, err := config.Resolve(t.Context(), set, keysOf{"botToken", "appToken", v1alpha1.GitHubWebhookSecretKey, v1alpha1.KourierV1SigningSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	inner := &spySink{}
	s := quietSink{renderSink: inner, quiet: noRepliesIn(func() *config.Snapshot { return snap })}

	for _, c := range []string{"platform/slack", "platform/github", "platform/v1", "platform/gone"} {
		key := session.Key{Connection: c, Channel: "ch-1", Thread: "t-1"}
		h := s.Turn(t.Context(), session.TurnRef{Key: key})
		if (h == nil) != (c == "platform/github") {
			t.Errorf("Turn on %s gave a handler = %v; want none on the connection with no replies alone", c, h != nil)
		}
		s.Notice(t.Context(), key, session.Notice{ID: "n-1", Kind: session.NoticeExpired})
		if err := s.QueueNotice(t.Context(), key, session.Notice{ID: "n-2", Kind: session.NoticeQueued}); err != nil {
			t.Errorf("QueueNotice on %s = %v; want nil", c, err)
		}
	}
	want := []string{
		"turn platform/slack", "notice platform/slack", "queue platform/slack",
		"turn platform/v1", "notice platform/v1", "queue platform/v1",
		"turn platform/gone", "notice platform/gone", "queue platform/gone",
	}
	if !slices.Equal(inner.got, want) {
		t.Errorf("the renderer's sink was given %q; want %q: nothing of the github connection's", inner.got, want)
	}

	if noRepliesIn(func() *config.Snapshot { return nil })("platform/github") {
		t.Error("with no config yet, a connection has no replies; want it to have them, and its rows to go to the outbox")
	}
}
