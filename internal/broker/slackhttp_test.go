package broker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// fakeSigning is the fake signing secret of the Slack connections in HTTP mode of these tests.
const fakeSigning = "fake-signing-secret"

// httpModeConn is the Slack connection ns/name in HTTP mode, with the keys of that mode.
func httpModeConn(name string) Connection {
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: "ns", Name: name}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{v1alpha1.SlackBotTokenKey: secret.New("xoxb-fake"), v1alpha1.SlackSigningSecretKey: secret.New(fakeSigning)}}
}

// someListener is a listener that stands for the chat listener: newPlatform only asks whether the broker has one.
func someListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// A Slack connection in HTTP mode is built on its bot token and signing secret, and no app token; a broker with no chat listener refuses to
// build it with a wiring error, since Slack could not reach it.
func TestNewPlatformInHTTPModeNeedsTheChatListener(t *testing.T) {
	var saw slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = sc; return idleSlack{} }}
	built, err := cfg.newPlatform(httpModeConn("team"), nil, nil, nil, nil)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") || built.Platform != nil {
		t.Errorf("newPlatform in HTTP mode with no chat listener = %+v, %v; want a wiring error that names AGENTKOURIER_CHAT_LISTEN", built, err)
	}
	cfg.ChatListener = someListener(t)
	if _, err := cfg.newPlatform(httpModeConn("team"), nil, nil, nil, nil); err != nil {
		t.Fatalf("newPlatform in HTTP mode = %v; want the Slack adapter", err)
	}
	if saw.BotToken.Reveal() != "xoxb-fake" || saw.SigningSecret.Reveal() != fakeSigning || !saw.AppToken.IsEmpty() {
		t.Errorf("the client was configured with bot %t, signing secret %t, app token %t; want the bot token and the signing secret alone",
			!saw.BotToken.IsEmpty(), !saw.SigningSecret.IsEmpty(), !saw.AppToken.IsEmpty())
	}
}

// A broker whose own connection is in Slack HTTP mode and that has no chat listener refuses to start, with a wiring error, before it opens
// the store or seeks to lead: a standby would otherwise start and say Ready, and fail only when it took over. With a chat listener, it
// starts.
func TestRunRefusesAnOwnConnectionInHTTPModeWithNoChatListener(t *testing.T) {
	inHTTP := func(t *testing.T) RunConfig {
		t.Helper()
		c := startConfig(t)
		conn := `apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-main, namespace: agent-kourier-system }
spec:
  platform: slack
  credentialsSecretRef: { name: slack-main }
  allowedNamespaces: ["sre", "payments"]
  slack: { mode: http }
`
		if err := os.WriteFile(filepath.Join(c.ConfigDir, "chatconnection.yaml"), []byte(conn), 0o600); err != nil {
			t.Fatal(err)
		}
		c.Client = fake.NewClientset(secretOf("agent-kourier-system", "slack-main", v1alpha1.SlackBotTokenKey, v1alpha1.SlackSigningSecretKey),
			secretOf("sre", "agent-kourier-sre-alerts-oidc", "token"))
		return c
	}

	c := inHTTP(t)
	c.DBPath = unmade(t)
	r := start(t, c)
	if err := r.wait(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") {
		t.Errorf("Run with no chat listener = %v; want ErrWiring naming AGENTKOURIER_CHAT_LISTEN", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after the refusal (%v): the check must come before the store", err)
	}

	c = inHTTP(t)
	c.ChatListener = someListener(t)
	r = start(t, c)
	r.until("/readyz", http.StatusOK)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run with a chat listener = %v after its context ended; want nil", err)
	}
}

// inboundSink records the messages a connection's run hands on.
type inboundSink struct {
	mu   sync.Mutex
	msgs []chat.Inbound
}

func (s *inboundSink) Message(_ context.Context, m chat.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *inboundSink) Interaction(context.Context, chat.Interaction) error { return nil }

func (s *inboundSink) got() []chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

// A Slack connection in HTTP mode runs as a runtime does, with the real adapter: once auth.test has passed its route on the chat listener
// takes Slack's signed requests and hands them to the connection's sink, refuses one that is not signed, and agentkourier_chat_connected says
// 1; once the runtime is halted, the route answers 503 and the series says 0.
func TestASlackConnectionInHTTPModeServesItsRouteWhileItRuns(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth.test" {
			_, _ = io.WriteString(w, `{"ok":true,"url":"https://fake.slack.com/","team_id":"TFAKE","user_id":"UFAKEBOT","bot_id":"BFAKEBOT"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
	}))
	t.Cleanup(api.Close)
	cfg := RunConfig{ChatListener: someListener(t), newSlack: func(sc slack.Config) slackAPI {
		sc.APIURL = api.URL + "/"
		return slack.New(sc)
	}}
	r := newHostRig(t, true)
	r.host.open = func(conn Connection, onConn func(bool)) (builtPlatform, error) {
		return cfg.newPlatform(conn, testutil.DiscardLogger(), onConn, nil, nil)
	}
	rt, err := r.host.build(httpModeConn("team"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &inboundSink{}
	r.host.run(rt, sink, nil)
	t.Cleanup(func() { r.host.haltAll(context.Background()); r.host.closeAll() })
	connected := func() float64 { return promtestutil.ToFloat64(r.m.chatUp.WithLabelValues("ns/team")) }
	testutil.Eventually(t, "the connection to be up", func() bool { return connected() == 1 }, testutil.Within(5*time.Second))

	body := `{"type":"event_callback","team_id":"TFAKE","event_id":"Ev1","event":{"type":"message","user":"U1","text":"hello","ts":"1.1","channel":"C1","channel_type":"channel"}}`
	post := func(sign bool) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.srv.URL+"/chat/slack/ns/team", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if sign {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(fakeSigning))
			_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := r.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(false); code != http.StatusUnauthorized {
		t.Errorf("an unsigned request = %d; want 401", code)
	}
	if code := post(true); code != http.StatusOK {
		t.Fatalf("a signed request = %d; want 200", code)
	}
	if got := sink.got(); len(got) != 1 || got[0].At.Conn != "ns/team" || got[0].At.Channel != "C1" || got[0].Text != "hello" {
		t.Errorf("the sink was given %+v; want the message, on the connection", got)
	}

	rt.halt()
	rt.wait(t.Context())
	if code := post(true); code != http.StatusServiceUnavailable {
		t.Errorf("a signed request once the runtime was halted = %d; want 503", code)
	}
	if got := connected(); got != 0 {
		t.Errorf("agentkourier_chat_connected once the runtime was halted = %v; want 0", got)
	}
}
