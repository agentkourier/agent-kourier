package broker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// spySink records what reaches the renderer's sink: each turn, notice and queued notice, by its connection. It is safe for
// concurrent use.
type spySink struct {
	mu  sync.Mutex
	got []string
}

func (s *spySink) add(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, what)
}

func (s *spySink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *spySink) Turn(_ context.Context, t session.TurnRef) agent.Handler {
	s.add("turn " + t.Key.Connection)
	return func(agent.Event) {}
}

func (s *spySink) Notice(_ context.Context, key session.Key, _ session.Notice) {
	s.add("notice " + key.Connection)
}

func (s *spySink) QueueNotice(_ context.Context, key session.Key, _ session.Notice) error {
	s.add("queue " + key.Connection)
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
	if got := inner.seen(); !slices.Equal(got, want) {
		t.Errorf("the renderer's sink was given %q; want %q: nothing of the github connection's", got, want)
	}

	if noRepliesIn(func() *config.Snapshot { return nil })("platform/github") {
		t.Error("with no config yet, a connection has no replies; want it to have them, and its rows to go to the outbox")
	}
}

// Run gives the session manager the quiet sink: a turn that a github connection's webhook starts reaches the agent, while the renderer's sink,
// which writes the outbox rows of turns and notices, is given nothing of it, and the outbox has no row once the broker has stopped.
func TestRunRendersNothingForAConnectionWithNoReplies(t *testing.T) {
	stub := stubagent.New()
	t.Cleanup(stub.Close)
	prompted := make(chan struct{}, 1)
	stub.Agent("kagent", "k8s-agent").Reply(func(string) []stubagent.Step {
		select {
		case prompted <- struct{}{}:
		default:
		}
		return []stubagent.Step{stubagent.Text("the review")}
	})

	c := startConfig(t)
	be := filepath.Join(c.ConfigDir, "agentbackend.yaml")
	raw, err := os.ReadFile(be)
	if err != nil {
		t.Fatal(err)
	}
	conn := `apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: gh, namespace: ns }
spec:
  platform: web
  credentialsSecretRef: { name: gh }
  allowedNamespaces: ["sre"]
  web: { profile: github, github: { login: pr-reviewer } }
`
	binding := `apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: reviews, namespace: sre }
spec:
  agent:
    backendRef: { namespace: agent-kourier-system, name: kagent-pilot }
    name: k8s-agent
    namespace: kagent
  identity: { userId: agent-kourier-sre-alerts, tokenSecretRef: { name: agent-kourier-sre-alerts-oidc } }
  chat:
    connectionRef: { namespace: ns, name: gh }
    channel: octo-org/hello-world
    triggers: [{ type: mention }]
`
	files := map[string]string{"agentbackend.yaml": strings.Replace(string(raw), "http://kagent-controller.kagent-pilot:8083", stub.URL(), 1),
		"chatconnection.yaml": conn, "binding.yaml": binding}
	for file, body := range files {
		if err := os.WriteFile(filepath.Join(c.ConfigDir, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(c.ConfigDir, "binding-minimal.yaml")); err != nil { // a Binding of the Slack connection this config has not
		t.Fatal(err)
	}
	oidc := secretOf("sre", "agent-kourier-sre-alerts-oidc", "token")
	oidc.Data["token"] = []byte(jwt("agentkourier@example.test"))
	c.Client = fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gh"},
		Data: map[string][]byte{v1alpha1.GitHubWebhookSecretKey: []byte(fakeWebhook)}}, oidc)
	c.ChatListener = someListener(t)
	sink := &spySink{}
	c.sink = sink

	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	base := "http://" + c.ChatListener.Addr().String()
	if code := statusOf(t, http.DefaultClient, signedGitHub(t, base, "gh", "pull_request", githubFixture(t, "pull_request.opened.json"), true)); code != http.StatusAccepted {
		t.Fatalf("the webhook = %d; want 202", code)
	}
	select {
	case <-prompted:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was never prompted: the webhook started no turn")
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended; want nil", err)
	}

	if got := sink.seen(); len(got) != 0 {
		t.Errorf("the renderer's sink was given %q; want nothing of a connection with no replies", got)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if rows, err := st.ListUnposted(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("the outbox holds %+v, %v; want no row", rows, err)
	}
}
