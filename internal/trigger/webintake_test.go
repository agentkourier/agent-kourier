package trigger_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/github"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/internal/trigger"
)

// The fake secrets of the web connections of these tests.
const (
	fakeGitHubWebhook  = "fake-github-webhook-secret"
	fakeKourierSigning = "fake-kourier-v1-signing-secret"
)

// webKeys is a SecretResolver whose Secrets have the keys of both web profiles.
type webKeys struct{}

func (webKeys) Keys(context.Context, string, string) ([]string, error) {
	return []string{v1alpha1.GitHubWebhookSecretKey, v1alpha1.KourierV1SigningSecretKey}, nil
}

// webIntake is the path of a web connection's events to the session manager, as the broker builds it: the web platform with the
// profile, running on an Intake over a config that binds channel to sre/web-agent on the connection agent-kourier-system/web, whose
// spec is spec. The Intake is told NoReplies from the platform, as the broker tells it. send hands the platform's handler a request
// and returns the status of its answer.
func webIntake(t *testing.T, spec v1alpha1.ChatConnectionSpec, profile web.Profile, channel string) (send func(*http.Request) int, sub *submitter) {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	ref := v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "web"}
	spec.AllowedNamespaces = []string{"sre"}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends: []v1alpha1.AgentBackend{{ObjectMeta: meta("agent-kourier-system", "kagent"),
			Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: []string{"sre"}}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta(ref.Namespace, ref.Name), Spec: spec}},
		Bindings: []v1alpha1.Binding{{ObjectMeta: meta("sre", "web-agent"), Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "kagent"}},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: ref, Channel: channel}}}},
	}, webKeys{})
	if err != nil {
		t.Fatal(err)
	}
	plat := platformOf(t, spec)
	sub = &submitter{}
	in := trigger.NewIntake(trigger.IntakeConfig{Connection: ref, Snapshot: func() *config.Snapshot { return snap }, Submitter: sub,
		NoReplies: !plat.Replies, Logger: testutil.DiscardLogger()})
	url := ""
	if plat.Replies {
		url = "https://receiver.example/kourier"
	}
	up := make(chan struct{}, 1)
	p, err := web.New(web.Config{Connection: "agent-kourier-system/web", Profile: profile, Platform: plat, URL: url, Logger: testutil.DiscardLogger(),
		OnConnection: func(connected bool) {
			if connected {
				up <- struct{}{}
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, in) }()
	t.Cleanup(func() { cancel(); _ = <-done })
	<-up // the platform takes requests
	h, _ := p.HTTPHandler()
	return func(req *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}, sub
}

// gitHubDelivery is a delivery of the event with body, signed with the webhook secret.
func gitHubDelivery(t *testing.T, event, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/web/agent-kourier-system/web", strings.NewReader(body))
	req.Header.Set(github.HeaderEvent, event)
	req.Header.Set(github.HeaderSignature, github.Sign([]byte(fakeGitHubWebhook), []byte(body)))
	return req
}

func gitHubFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "chat", "web", "github", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// platformOf is what the API says a connection of spec takes, which the broker hands its profile and its web platform.
func platformOf(t *testing.T, spec v1alpha1.ChatConnectionSpec) v1alpha1.ChatPlatform {
	t.Helper()
	plat, ok := v1alpha1.ChatPlatformOf(spec)
	if !ok {
		t.Fatalf("ChatPlatformOf(%+v) = false", spec)
	}
	return plat
}

// Every event of one pull request is a turn of one session: its opening, a comment that mentions the login and a push to it are
// submitted under one key (the repository, and pr- and the number), each with a MessageID of its own, each allowed to create the session
// and each saying the platform has no replies, so that a turn that comes after the session expired starts a fresh one. GitHub's
// redelivery of a body the intake took is dropped there, before the session manager is asked.
func TestTheEventsOfAPullRequestAreTurnsOfOneSession(t *testing.T) {
	spec := v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWeb, CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds"},
		Web: &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileGitHub, Identity: "pr-reviewer"}}
	prof, err := github.New(secret.New(fakeGitHubWebhook), spec.Web.Identity, platformOf(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	send, sub := webIntake(t, spec, prof, "octo-org/hello-world")

	opened := gitHubFixture(t, "pull_request.opened.json")
	pushed := strings.Replace(opened, `"action": "opened"`, `"action": "synchronize"`, 1)
	if pushed == opened {
		t.Fatal("the fixture's action is not where the test looks for it")
	}
	comment := gitHubFixture(t, "issue_comment.created.json")
	for _, d := range []struct{ event, body string }{
		{"pull_request", opened}, {"issue_comment", comment}, {"pull_request", pushed},
		{"issue_comment", comment}, // GitHub's redelivery of the comment: the same body
	} {
		if code := send(gitHubDelivery(t, d.event, d.body)); code != http.StatusAccepted {
			t.Fatalf("a %s delivery was answered %d, want 202", d.event, code)
		}
	}

	got := sub.submitted()
	if len(got) != 3 {
		t.Fatalf("submitted %d turns, want 3: the opening, the comment and the push, and not the comment again: %+v", len(got), got)
	}
	key := session.Key{Connection: "agent-kourier-system/web", Channel: "octo-org/hello-world", Thread: "pr-42"}
	ids := map[string]bool{}
	for i, in := range got {
		if in.Key != key || !in.Create || in.Binding != "sre/web-agent" || !in.Mentioned || !in.NoReplies {
			t.Errorf("turn %d = %+v; want key %+v, allowed to create under sre/web-agent, mentioned, and with no replies", i, in, key)
		}
		ids[in.MessageID] = true
	}
	if len(ids) != 3 {
		t.Errorf("the turns' MessageIDs are %v; want three, one for each event", ids)
	}
}

// kourierV1Message is a message to a kourier-v1 connection, signed with its secret now.
func kourierV1Message(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/web/agent-kourier-system/web", strings.NewReader(body))
	ts, sig := kourierv1.Sign([]byte(fakeKourierSigning), time.Now(), []byte(body))
	req.Header.Set(kourierv1.HeaderTimestamp, ts)
	req.Header.Set(kourierv1.HeaderSignature, sig)
	return req
}

// A kourier-v1 message that opens a thread and one in its thread are turns of one session: the thread is the first message's ID, each
// has its own MessageID, and a message sent again with the same id is dropped at the intake. The profile posts replies, so the turns do
// not say otherwise: an ended session there is told so, as on Slack.
func TestTheMessagesOfAKourierV1ThreadAreTurnsOfOneSession(t *testing.T) {
	spec := v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformWeb, CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds"},
		Web: &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileKourierV1, URL: "https://receiver.example/kourier"}}
	prof, err := kourierv1.New(secret.New(fakeKourierSigning), platformOf(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	send, sub := webIntake(t, spec, prof, "ch-1")

	first := `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hello"}`
	reply := `{"v":1,"channel":"ch-1","id":"m-2","thread":"m-1","user":"u-2","text":"and this?","mention":true}`
	for _, body := range []string{first, reply, reply} {
		if code := send(kourierV1Message(t, body)); code != http.StatusAccepted {
			t.Fatalf("a message was answered %d, want 202", code)
		}
	}

	got := sub.submitted()
	if len(got) != 2 {
		t.Fatalf("submitted %d turns, want 2: the message and its reply, and not the reply again: %+v", len(got), got)
	}
	key := session.Key{Connection: "agent-kourier-system/web", Channel: "ch-1", Thread: "m-1"}
	for i, in := range got {
		if in.Key != key || !in.Create || in.Binding != "sre/web-agent" || in.NoReplies {
			t.Errorf("turn %d = %+v; want key %+v, allowed to create under sre/web-agent, on a platform with replies", i, in, key)
		}
	}
	if got[0].MessageID == got[1].MessageID {
		t.Errorf("both turns have the MessageID %q; want one each", got[0].MessageID)
	}
}
