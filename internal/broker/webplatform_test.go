package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/github"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The fake secrets of the web connections of these tests.
const (
	fakeWebSigning = "fake-kourier-v1-signing-secret"
	fakeWebhook    = "fake-github-webhook-secret"
)

// kourierV1Conn is the web connection ns/name of the kourier-v1 profile, which posts its replies to url.
func kourierV1Conn(name, url string) Connection {
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: name}, Platform: v1alpha1.PlatformWeb,
		Web:         &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileKourierV1, URL: url},
		Credentials: map[string]secret.Value{v1alpha1.KourierV1SigningSecretKey: secret.New(fakeWebSigning)}}
}

// githubConn is the web connection ns/name of the github profile, for the agent that posts as pr-reviewer.
func githubConn(name string) Connection {
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: name}, Platform: v1alpha1.PlatformWeb,
		Web:         &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileGitHub, GitHub: &v1alpha1.GitHubWebSpec{Login: "pr-reviewer"}},
		Credentials: map[string]secret.Value{v1alpha1.GitHubWebhookSecretKey: secret.New(fakeWebhook)}}
}

// A web connection is built as the web platform, with its profile made from its spec and from the key of its Secret that the profile
// reads: kourier-v1 sends replies, and github none. A broker with no chat listener refuses to build either, with a wiring error that names
// AGENTKOURIER_CHAT_LISTEN, since its sender could not reach it; and a spec its profile cannot run with is a wiring error too.
func TestNewPlatformBuildsAWebConnectionOfEachProfile(t *testing.T) {
	var none RunConfig
	for _, conn := range []Connection{kourierV1Conn("v1", "https://receiver.example.com/replies"), githubConn("gh")} {
		built, err := none.newPlatform(conn, nil, nil)
		if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") || built.Platform != nil {
			t.Errorf("%s with no chat listener: newPlatform = %+v, %v; want a wiring error that names AGENTKOURIER_CHAT_LISTEN", conn.Web.Profile, built, err)
		}
		if err := none.needsChatListener(conn); !errors.Is(err, ErrWiring) {
			t.Errorf("%s: needsChatListener with no chat listener = %v; want a wiring error, which Run checks on every replica", conn.Web.Profile, err)
		}
	}

	cfg := RunConfig{ChatListener: someListener(t)}
	v1, err := cfg.newPlatform(kourierV1Conn("v1", "https://receiver.example.com/replies"), nil, nil)
	if err != nil || v1.closeIdle == nil {
		t.Fatalf("newPlatform(kourier-v1) = %+v, %v; want the web platform, and a way to close its connections", v1, err)
	}
	if _, ok := v1.Platform.(*web.Platform); !ok {
		t.Errorf("newPlatform(kourier-v1) built a %T; want the web platform", v1.Platform)
	}
	if _, ok := receiverOf(v1.Platform); !ok {
		t.Error("the kourier-v1 connection offers no handler for the chat listener")
	}
	if got := v1.Capabilities(); got.MaxText == 0 || got.Streams || got.AckReaction != "" {
		t.Errorf("the kourier-v1 connection's capabilities are %+v; want the profile's limits and nothing else", got)
	}
	gh, err := cfg.newPlatform(githubConn("gh"), nil, nil)
	if err != nil {
		t.Fatalf("newPlatform(github) = %v; want the web platform", err)
	}
	if _, err := gh.Post(t.Context(), chat.Target{Conn: "ns/gh", Channel: "octo-org/hello-world"}, chat.Say(chat.Message{Fallback: "x"}), "tag"); !errors.Is(err, web.ErrNoReplies) {
		t.Errorf("a post on the github connection = %v; want ErrNoReplies: the profile sends none", err)
	}

	for name, conn := range map[string]Connection{
		"no spec.web":            {Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: "w"}, Platform: v1alpha1.PlatformWeb},
		"github with no login":   func() Connection { c := githubConn("gh"); c.Web.GitHub = nil; return c }(),
		"kourier-v1 with no URL": kourierV1Conn("v1", ""),
		"no signing secret":      func() Connection { c := kourierV1Conn("v1", "https://r.example/"); c.Credentials = nil; return c }(),
		"an unknown profile":     func() Connection { c := githubConn("gh"); c.Web.Profile = "gitlab"; return c }(),
	} {
		built, err := cfg.newPlatform(conn, nil, nil)
		if !errors.Is(err, ErrWiring) || built.Platform != nil {
			t.Errorf("%s: newPlatform = %+v, %v; want a wiring error and no platform", name, built, err)
		}
		if err != nil && (strings.Contains(err.Error(), fakeWebSigning) || strings.Contains(err.Error(), fakeWebhook)) {
			t.Errorf("%s: the error %q holds the secret", name, err)
		}
	}
}

// signedKourierV1 is a kourier-v1 request to the connection's route, signed with the connection's secret now (or not at all).
func signedKourierV1(t *testing.T, base, conn, body string, sign bool) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/chat/web/ns/"+conn, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if sign {
		ts, sig := kourierv1.Sign([]byte(fakeWebSigning), time.Now(), []byte(body))
		req.Header.Set(kourierv1.HeaderTimestamp, ts)
		req.Header.Set(kourierv1.HeaderSignature, sig)
	}
	return req
}

// signedGitHub is a GitHub delivery of the event to the connection's route, signed with the connection's webhook secret (or not at all).
func signedGitHub(t *testing.T, base, conn, event, body string, sign bool) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/chat/web/ns/"+conn, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(github.HeaderEvent, event)
	if sign {
		req.Header.Set(github.HeaderSignature, github.Sign([]byte(fakeWebhook), []byte(body)))
	}
	return req
}

// statusOf sends req and returns the status of its answer.
func statusOf(t *testing.T, c *http.Client, req *http.Request) int {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// githubFixture is the body of the github profile's fixture name (internal/chat/web/github/testdata), as GitHub's documentation shows it.
func githubFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "chat", "web", "github", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A web connection runs as a runtime does, with the real platform and profile: while it runs its route on the chat listener takes its
// sender's signed requests and hands them to the connection's sink, refuses one that is not signed, and agentkourier_chat_connected says
// 1; once the runtime is halted, the route answers 503 and the series says 0. Both profiles, each with its own signature.
func TestAWebConnectionServesItsRouteWhileItRuns(t *testing.T) {
	for _, tc := range []struct {
		profile v1alpha1.WebProfile
		conn    Connection
		request func(t *testing.T, base string, sign bool) *http.Request
		want    chat.Inbound
	}{
		{v1alpha1.WebProfileKourierV1, kourierV1Conn("v1", "https://receiver.example.com/replies"),
			func(t *testing.T, base string, sign bool) *http.Request {
				return signedKourierV1(t, base, "v1", `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hello"}`, sign)
			},
			chat.Inbound{At: chat.Ref{Platform: "web", Conn: "ns/v1", Channel: "ch-1", ID: "m-1"}, User: "u-1", Text: "hello", Mentioned: true}},
		{v1alpha1.WebProfileGitHub, githubConn("gh"),
			func(t *testing.T, base string, sign bool) *http.Request {
				return signedGitHub(t, base, "gh", "pull_request", githubFixture(t, "pull_request.opened.json"), sign)
			},
			chat.Inbound{At: chat.Ref{Platform: "web", Conn: "ns/gh", Channel: "octo-org/hello-world", Thread: "pr-42"}, User: "octocat", Mentioned: true}},
	} {
		t.Run(string(tc.profile), func(t *testing.T) {
			cfg := RunConfig{ChatListener: someListener(t)}
			r := newHostRig(t, true)
			r.host.open = func(conn Connection, onConn func(bool)) (builtPlatform, error) {
				return cfg.newPlatform(conn, testutil.DiscardLogger(), onConn)
			}
			rt, err := r.host.build(tc.conn)
			if err != nil {
				t.Fatal(err)
			}
			sink := &inboundSink{}
			r.host.run(rt, sink, nil)
			t.Cleanup(func() { r.host.haltAll(context.Background()); r.host.closeAll() })
			name := tc.conn.Ref.Namespace + "/" + tc.conn.Ref.Name
			connected := func() float64 { return promtestutil.ToFloat64(r.m.chatUp.WithLabelValues(name)) }
			testutil.Eventually(t, "the connection to be up", func() bool { return connected() == 1 }, testutil.Within(5*time.Second))

			client := r.srv.Client()
			if code := statusOf(t, client, tc.request(t, r.srv.URL, false)); code != http.StatusUnauthorized {
				t.Errorf("an unsigned request = %d; want 401", code)
			}
			if code := statusOf(t, client, tc.request(t, r.srv.URL, true)); code != http.StatusAccepted {
				t.Fatalf("a signed request = %d; want 202", code)
			}
			got := sink.got()
			if len(got) != 1 {
				t.Fatalf("the sink was given %d messages; want 1", len(got))
			}
			m := got[0]
			if tc.profile == v1alpha1.WebProfileGitHub { // its ID is the body's hash and its text the profile's own: the rest is what matters here
				m.At.ID, m.Text = "", ""
			}
			if m != tc.want {
				t.Errorf("the sink was given %+v; want %+v", got[0], tc.want)
			}

			rt.halt()
			rt.wait(t.Context())
			if code := statusOf(t, client, tc.request(t, r.srv.URL, true)); code != http.StatusServiceUnavailable {
				t.Errorf("a signed request once the runtime was halted = %d; want 503", code)
			}
			if got := connected(); got != 0 {
				t.Errorf("agentkourier_chat_connected once the runtime was halted = %v; want 0", got)
			}
		})
	}
}

// A kourier-v1 connection posts a reply, through the platform every part of the broker gets for it, to the URL of its spec, signed with its
// secret and with the post's tag as its idempotency key; and the transport the runtime closes at shutdown is the one the reply went over.
func TestAKourierV1ConnectionPostsItsRepliesToItsURL(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	cfg := RunConfig{ChatListener: someListener(t)}
	r := newHostRig(t, true)
	r.host.open = func(conn Connection, onConn func(bool)) (builtPlatform, error) {
		return cfg.newPlatform(conn, testutil.DiscardLogger(), onConn)
	}
	rt, err := r.host.build(kourierV1Conn("v1", receiver.URL+"/replies"))
	if err != nil {
		t.Fatal(err)
	}
	r.host.run(rt, &inboundSink{}, nil)
	t.Cleanup(func() { r.host.haltAll(context.Background()); r.host.closeAll() })

	p, err := r.host.table.Platform("ns/v1")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := p.Post(t.Context(), chat.Target{Conn: "ns/v1", Channel: "ch-1", Thread: "m-1"}, chat.Say(chat.Message{Fallback: "the answer"}), "outbox-tag-1")
	if err != nil {
		t.Fatalf("Post = %v; want the reply sent", err)
	}
	if ref.ID != "outbox-tag-1" || ref.Conn != "ns/v1" {
		t.Errorf("Post = %+v; want the reply's ref on the connection, by its tag", ref)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the receiver got %d requests; want 1", len(got))
	}
	req := got[0]
	if req.URL.Path != "/replies" || req.Header.Get(kourierv1.HeaderIdempotencyKey) != "outbox-tag-1" {
		t.Errorf("the reply went to %s with the key %q; want /replies with the post's tag", req.URL.Path, req.Header.Get(kourierv1.HeaderIdempotencyKey))
	}
	if err := kourierv1.VerifySignature([]byte(fakeWebSigning), time.Now(), req.Header.Get(kourierv1.HeaderTimestamp), req.Header.Get(kourierv1.HeaderSignature), []byte(bodies[0])); err != nil {
		t.Errorf("the reply is not signed with the connection's secret: %v", err)
	}
}

// An own web connection needs the chat listener, since its sender sends to the listener's route: with none, Run refuses with a wiring error that
// names the setting, before it opens the store. With one, the broker runs and is ready.
func TestRunNeedsAChatListenerForAnOwnWebConnection(t *testing.T) {
	onWebConfig := func(t *testing.T) RunConfig {
		t.Helper()
		c := startConfig(t)
		conn := `apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-main, namespace: agent-kourier-system }
spec:
  platform: web
  credentialsSecretRef: { name: slack-main }
  allowedNamespaces: ["sre", "payments"]
  web: { profile: kourier-v1, url: "https://receiver.example/kourier" }
`
		// The example Binding asks a person questions and shows feedback buttons, which the kourier-v1 profile does not have: this one does not.
		binding := `apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: sre-alerts, namespace: sre }
spec:
  agent:
    backendRef: { namespace: agent-kourier-system, name: kagent-pilot }
    name: k8s-agent
    namespace: kagent
  identity: { userId: agent-kourier-sre-alerts }
  chat: { connectionRef: { namespace: agent-kourier-system, name: slack-main }, channel: sre-alerts-1 }
`
		for file, body := range map[string]string{"chatconnection.yaml": conn, "binding.yaml": binding} {
			if err := os.WriteFile(filepath.Join(c.ConfigDir, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c.Client = fake.NewClientset(secretOf("agent-kourier-system", "slack-main", v1alpha1.KourierV1SigningSecretKey))
		return c
	}

	c := onWebConfig(t)
	c.DBPath = unmade(t)
	r := start(t, c)
	if err := r.wait(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") {
		t.Errorf("Run with no chat listener = %v; want ErrWiring naming AGENTKOURIER_CHAT_LISTEN", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after the refusal (%v): the check must come before the store", err)
	}

	c = onWebConfig(t)
	c.ChatListener = someListener(t)
	r = start(t, c)
	r.until("/readyz", http.StatusOK)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run with a chat listener = %v after its context ended; want nil", err)
	}
}
