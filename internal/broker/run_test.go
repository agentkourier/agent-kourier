package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// startConfig is a RunConfig that would run: the example config in a temp dir, a fake cluster that
// holds its Secrets, and a free port for the health server.
func startConfig(t *testing.T) RunConfig {
	t.Helper()
	c := valid(t)
	c.ConfigDir = configDir(t)
	c.DBPath = filepath.Join(t.TempDir(), "courier.db")
	c.Client = cluster()
	c.SyncTimeout = 30 * time.Second
	c.newSlack = func(slack.Config) slackAPI { return idleSlack{} } // offline: the default build cannot reach a fake
	return c
}

// idleSlack is a Slack client that holds its connection until its context ends and calls nothing.
type idleSlack struct{}

func (idleSlack) Run(ctx context.Context, _ slack.Sink) error { <-ctx.Done(); return ctx.Err() }

func (idleSlack) Post(context.Context, string, string, slack.Content, string) (string, error) {
	return "", errors.New("idleSlack posts nothing")
}

func (idleSlack) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	return "", false, nil
}

func (idleSlack) ResolveUser(context.Context, string) (dialect.User, error) {
	return dialect.User{}, errors.New("idleSlack knows no one")
}

// The rest of what Run asks of its Slack client: edits, streams, ephemerals, membership and reactions.
// idleSlack answers each as a Slack that has nothing against it would, and calls nothing.
func (idleSlack) Update(context.Context, string, string, slack.Content) error { return nil }

func (idleSlack) StartStream(context.Context, string, string, string, string) (string, error) {
	return "1.0", nil
}
func (idleSlack) AppendStream(context.Context, string, string, string) error { return nil }
func (idleSlack) StopStream(context.Context, string, string, string, json.RawMessage, string) error {
	return nil
}
func (idleSlack) PostEphemeral(context.Context, string, string, string, slack.Content) error {
	return nil
}
func (idleSlack) IsChannelMember(context.Context, string, string) (bool, error) { return false, nil }
func (idleSlack) IsBot(context.Context, string) (bool, error)                   { return false, nil }
func (idleSlack) React(context.Context, string, string, string) error           { return nil }

// revokedReacts is idleSlack whose reactions fail the way a revoked bot token fails them.
type revokedReacts struct{ idleSlack }

func (revokedReacts) React(context.Context, string, string, string) error {
	return slackErr("token_revoked")
}

// revokedSlack is idleSlack whose posts fail the way a revoked bot token fails them.
type revokedSlack struct{ idleSlack }

func (revokedSlack) Post(context.Context, string, string, slack.Content, string) (string, error) {
	return "", slackErr("token_revoked")
}

func (revokedSlack) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	return "", false, slackErr("token_revoked")
}

// A bot token revoked while the connection is up fails the posts the outbox makes, which the outbox
// treats as transient. Run must still end with ErrSlackAuth: here a post left unsent by an earlier run
// is what finds the token revoked.
func TestRunEndsWithSlackAuthWhenAPostFindsTheTokenRevoked(t *testing.T) {
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return revokedSlack{} }
	seedPost(t, c)
	r := start(t, c)

	err := r.wait()

	if !errors.Is(err, ErrSlackAuth) || !strings.Contains(err.Error(), "token_revoked") {
		t.Errorf("Run = %v, want ErrSlackAuth naming token_revoked, from the post", err)
	}
}

// Reactions are made by the outbox through the same client, and a revoked token finds them out the same
// way: here a reaction row an earlier run left is what finds the token revoked (D6).
func TestRunEndsWithSlackAuthWhenAReactionFindsTheTokenRevoked(t *testing.T) {
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return revokedReacts{} }
	seedReaction(t, c)
	r := start(t, c)

	err := r.wait()

	if !errors.Is(err, ErrSlackAuth) || !strings.Contains(err.Error(), "token_revoked") {
		t.Errorf("Run = %v, want ErrSlackAuth naming token_revoked, from the reaction", err)
	}
}

// What Run hands the Slack client is the connection's own tokens, read at startup, and an API URL
// that a production build can never set: the environment variable that a test image reads is
// ignored here, so a bot token cannot be redirected.
func TestRunBuildsTheSlackConfigFromTheConnectionSecret(t *testing.T) {
	t.Setenv(testhooks.SlackAPIURLEnv, "http://evil.example/")
	var got slack.Config
	c := startConfig(t)
	c.newSlack = func(sc slack.Config) slackAPI { got = sc; return idleSlack{} }
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	if got.BotToken.Reveal() != "fake-value" || got.AppToken.Reveal() != "fake-value" {
		t.Error("the Slack config does not carry the tokens of the connection's Secret")
	}
	if got.AuthzTimeout != interact.PressAuthzTimeout {
		t.Errorf("AuthzTimeout = %s, want the %s a press is given for its membership checks", got.AuthzTimeout, interact.PressAuthzTimeout)
	}
	if wantURL := testhooks.SlackAPIURL(); got.APIURL != wantURL {
		t.Errorf("APIURL = %q, want %q: the testhooks value, and nothing from the environment in any other build", got.APIURL, wantURL)
	}
}

// running is a Run in progress.
type running struct {
	t      *testing.T
	cfg    RunConfig
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, c RunConfig) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{t: t, cfg: c, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- Run(ctx, c) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

// wait returns Run's error, or fails the test if Run has not returned in 10 s.
func (r *running) wait() error {
	r.t.Helper()
	return r.waitWithin(10 * time.Second)
}

// waitWithin returns Run's error, or fails the test if Run has not returned within d.
func (r *running) waitWithin(d time.Duration) error {
	r.t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup, which waits too
		return err
	case <-time.After(d):
		r.t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

// get asks the health server for path.
func (r *running) get(path string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodGet, "http://"+r.cfg.Listener.Addr().String()+path, http.NoBody)
	if err != nil {
		r.t.Fatal(err)
	}
	// A listener nobody serves on still accepts connections, so a request needs a deadline of its own.
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// until polls get(path) until it answers want, or fails after 10 s.
func (r *running) until(path string, want int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := r.get(path)
		if code == want {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s = %d %q, want %d", path, code, body, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Liveness and readiness are different questions: the process is alive as soon as it serves, and
// ready only when the Secret caches have synced and the store answers.
func TestRunAnswersHealthzWhileReadyzIsStillStarting(t *testing.T) {
	gate := make(chan struct{})
	var open sync.Once
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-gate // the Secret caches cannot sync, so Bootstrap cannot finish
		return false, nil, nil
	})
	r := start(t, c)
	t.Cleanup(func() { open.Do(func() { close(gate) }) }) // after start's own, so it runs first: Run cannot return while Bootstrap waits

	r.until("/healthz", http.StatusOK)
	if code, body := r.get("/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "starting") {
		t.Errorf("/readyz during Bootstrap = %d %q, want 503 starting", code, body)
	}

	open.Do(func() { close(gate) })
	r.until("/readyz", http.StatusOK)
	if code, _ := r.get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz after readiness = %d", code)
	}
}

// Before the first config load nothing is known to be fresh, so the gauge says stale: exporting 0 there
// would claim a state courier has not reached, and an alert could key on it.
func TestRunExportsTheConfigAsStaleUntilItHasLoaded(t *testing.T) {
	gate := make(chan struct{})
	var open sync.Once
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-gate
		return false, nil, nil
	})
	r := start(t, c)
	t.Cleanup(func() { open.Do(func() { close(gate) }) })

	r.until("/healthz", http.StatusOK)
	if _, body := r.get("/metrics"); !strings.Contains(body, "courier_config_stale 1\n") {
		t.Errorf("a scrape before Bootstrap finishes lacks courier_config_stale 1:\n%s", body)
	}
	open.Do(func() { close(gate) })
	r.until("/readyz", http.StatusOK)
	if _, body := r.get("/metrics"); !strings.Contains(body, "courier_config_stale 0\n") {
		t.Errorf("a scrape after the first load lacks courier_config_stale 0:\n%s", body)
	}
}

// A signal during startup, here while Bootstrap waits for the Secret caches, is a clean stop, and it
// aborts the wait: the caches never sync here, so Run must not be left to run out SyncTimeout (30 s in
// this test, and 40 s of grace in the Deployment) before it notices. A denial that comes while the
// context is live is not a clean stop, and stays distinct from a cancellation.
func TestRunStopsCleanlyWhenTheContextEndsDuringStartup(t *testing.T) {
	before := goleak.IgnoreCurrent()
	gate := make(chan struct{})
	var open sync.Once
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-gate // never opened until Run has returned
		return false, nil, nil
	})
	r := start(t, c)
	t.Cleanup(func() { open.Do(func() { close(gate) }) })
	r.until("/healthz", http.StatusOK) // Run is in Bootstrap

	r.cancel()

	if err := r.waitWithin(2 * time.Second); err != nil {
		t.Errorf("Run = %v after a signal during startup, want nil", err)
	}
	open.Do(func() { close(gate) })
	goleak.VerifyNone(t, before)
}

func TestRunReturnsNilWhenItsContextEndsAndLeavesNothingRunning(t *testing.T) {
	before := goleak.IgnoreCurrent()
	r := start(t, startConfig(t))
	r.until("/readyz", http.StatusOK)

	r.cancel()

	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after its context ended, want nil: a clean shutdown", err)
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", r.cfg.Listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Error("the health server still accepts connections after Run returned")
	}
	goleak.VerifyNone(t, before)
}

// The connection check comes right after Bootstrap, before the store exists: a config that v1 cannot
// serve leaves no database behind and never becomes ready.
func TestRunRefusesTwoConnectionsBeforeItOpensTheStore(t *testing.T) {
	c := startConfig(t)
	second := `apiVersion: agentcourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-two, namespace: courier-system }
spec: { platform: slack, mode: socket, credentialsSecretRef: { name: slack-two } }
`
	if err := os.WriteFile(filepath.Join(c.ConfigDir, "second.yaml"), []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Client = fake.NewClientset(secretOf("courier-system", "slack-main", "botToken", "appToken"), secretOf("courier-system", "slack-two", "botToken", "appToken"),
		secretOf("sre", "courier-sre-alerts-oidc", "token"), secretOf("sre", "courier-webhook-token", "token"))
	r := start(t, c)

	if err := r.wait(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "slack-two") {
		t.Errorf("Run = %v, want ErrWiring naming both connections", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after a wiring error (%v): the connection check must come before the store", err)
	}
}

func TestRunPassesAnRBACDenialThroughAsAccessDenied(t *testing.T) {
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: no list"))
	})
	r := start(t, c)

	err := r.wait()

	if !errors.Is(err, k8ssecrets.ErrAccessDenied) || !apierrors.IsForbidden(err) || errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v, want ErrAccessDenied wrapping the forbidden cause, and not a wiring error", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("the database was opened although Bootstrap failed")
	}
}

func TestRunRefusesAnIncompleteConfigBeforeStartingAnything(t *testing.T) {
	c := startConfig(t)
	c.Interactions = nil
	r := start(t, c)

	if err := r.wait(); !errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v, want ErrWiring", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("the database was opened for an incomplete config")
	}
}

// klog is routed before anything else, so client-go's first log line already goes through the
// cap and to the broker's logger.
func TestRunRoutesKlogToItsLoggerFirst(t *testing.T) {
	t.Cleanup(klog.ClearLogger)
	out := &logBuffer{}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.Level(-20)}))
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	klog.Info("klog probe line")

	if !strings.Contains(out.String(), "klog probe line") {
		t.Errorf("klog output did not reach the broker's logger: %q", out.String())
	}
}
