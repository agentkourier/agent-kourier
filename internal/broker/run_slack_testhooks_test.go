//go:build testhooks

package broker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// These tests reach Slack, which only a testhooks build can point at the fake: the default build
// cannot redirect the Slack URL, by design (TestSlackConfigTakesItsAPIURLOnlyFromTestHooks).

// turns is a session.Sink that records what the agent said, so a test can see a turn run.
type turns struct {
	mu      sync.Mutex
	texts   []string
	notices []session.NoticeKind
	seen    chan struct{}
}

// noticed says whether a notice of kind has been handed to the sink.
func (s *turns) noticed(kind session.NoticeKind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.notices, kind)
}

func (s *turns) Turn(context.Context, session.TurnRef) agent.Handler {
	return func(e agent.Event) {
		if te, ok := e.(agent.TextEvent); ok {
			s.mu.Lock()
			s.texts = append(s.texts, te.Text)
			s.mu.Unlock()
			select {
			case s.seen <- struct{}{}:
			default:
			}
		}
	}
}

func (s *turns) Notice(ctx context.Context, key session.Key, n session.Notice) {
	_ = s.QueueNotice(ctx, key, n)
}

// QueueNotice is what the Slack intake raises its notices through.
func (s *turns) QueueNotice(_ context.Context, _ session.Key, n session.Notice) error {
	s.mu.Lock()
	s.notices = append(s.notices, n.Kind)
	s.mu.Unlock()
	return nil
}

// slackWorld is the example config wired to a fake Slack and a stub agent, as Run sees them.
type slackWorld struct {
	cfg   RunConfig
	fake  *fakeslack.Server
	sink  *turns
	stub  *stubagent.Server
	botTk string
}

func newSlackWorld(t *testing.T, botToken string) *slackWorld {
	t.Helper()
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	stub := stubagent.New()
	t.Cleanup(stub.Close)
	stub.Agent("kagent", "k8s-agent").Reply(func(string) []stubagent.Step { return []stubagent.Step{stubagent.Text("the answer")} })
	t.Setenv(testhooks.SlackAPIURLEnv, fake.URL()+"/")

	c := startConfig(t)
	be := filepath.Join(c.ConfigDir, "agentbackend.yaml")
	raw, err := os.ReadFile(be)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(be, []byte(strings.Replace(string(raw), "http://kagent-controller.kagent-pilot:8083", stub.URL(), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Client = slackCluster(botToken)
	c.newSlack = nil // the real client, against the fake
	sink := &turns{seen: make(chan struct{}, 8)}
	c.sink = sink
	return &slackWorld{cfg: c, fake: fake, sink: sink, stub: stub, botTk: botToken}
}

// rejectedWorld is a world whose Binding token the stub refuses (a malformed bearer is a 401), so
// the first turn finds its credentials rejected.
func rejectedWorld(t *testing.T) *slackWorld {
	t.Helper()
	w := newSlackWorld(t, "xoxb-fake-bot")
	conn := secretOf("courier-system", "slack-main", "appToken")
	conn.Data["botToken"], conn.Data["appToken"] = []byte("xoxb-fake-bot"), []byte("xapp-fake-app")
	oidc := secretOf("sre", "courier-sre-alerts-oidc", "token")
	oidc.Data["token"] = []byte("not-a-jwt")
	w.cfg.Client = fake.NewClientset(conn, oidc, secretOf("sre", "courier-webhook-token", "token"))
	return w
}

// mention sends the mention that starts a turn, once Run is connected.
func (w *slackWorld) mention(t *testing.T, r *running) {
	t.Helper()
	r.until("/readyz", http.StatusOK)
	waitConnected(t, w.fake)
	if _, err := w.fake.Mention("C0123456789", "U1", "<@UFAKEBOT> why is it down?"); err != nil {
		t.Fatal(err)
	}
}

func TestRunCountsACredentialTheAgentRejected(t *testing.T) {
	w := rejectedWorld(t)
	r := start(t, w.cfg)

	w.mention(t, r)

	r.waitMetric(`courier_credential_rejections_total{binding="sre/sre-alerts"} 1`)
}

// failingAudit is a store whose audit writes fail, which a metric must say.
type failingAudit struct{ store.Audit }

func (failingAudit) AppendAudit(context.Context, store.AuditEntry) error {
	return errors.New("disk full")
}

func TestRunCountsAnAuditWriteThatFailed(t *testing.T) {
	w := rejectedWorld(t) // a rejected credential is audited
	w.cfg.auditStore = func(s store.Audit) store.Audit { return failingAudit{s} }
	r := start(t, w.cfg)

	w.mention(t, r)

	r.waitMetric(`courier_audit_write_failures_total{kind="agent_call"} 1`)
}

// With no sink of its own, Run renders turns itself, through the Slack client and its own outbox: the
// agent's answer ends up in the thread.
func TestRunPostsTheAgentsAnswerThroughItsOwnRenderer(t *testing.T) {
	w := newSlackWorld(t, "xoxb-fake-bot")
	w.cfg.sink = nil
	r := start(t, w.cfg)

	w.mention(t, r)

	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		for _, m := range w.fake.Messages("C0123456789") {
			if m.BotID != "" && strings.Contains(m.Text, "the answer") && !m.Streaming {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent's answer never appeared in the channel: %+v", w.fake.Messages("C0123456789"))
		}
	}
}

// A thread that has expired is told so by the notice the intake raises, which Run connects to the same
// sink as the turns' output: here the notice is what the test's sink is handed.
func TestRunConnectsTheIntakesNoticesToTheSink(t *testing.T) {
	w := newSlackWorld(t, "xoxb-fake-bot")
	r := start(t, w.cfg)
	w.mention(t, r)
	select {
	case <-w.sink.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the first turn never ran")
	}
	first := w.fake.Messages("C0123456789")
	if len(first) == 0 {
		t.Fatal("the mention is not in the channel")
	}
	thread := first[0].TS
	// The thread has been quiet for longer than any threadTTL.
	st, err := sqlite.Open(t.Context(), w.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "courier-system/slack-main", Channel: "C0123456789", ThreadTS: thread}
	if err := st.TouchSession(t.Context(), key, time.Now().Add(-10000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := w.fake.Reply("C0123456789", thread, "U1", "still there?"); err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(10 * time.Second); !w.sink.noticed(session.NoticeExpired); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the intake's expiry notice never reached the sink: IntakeConfig.Notice is not connected")
		}
	}
}

func TestRunTakesAMentionFromSlackThroughTheSessionToTheAgent(t *testing.T) {
	w := newSlackWorld(t, "xoxb-fake-bot")
	before := goleak.IgnoreCurrent()
	r := start(t, w.cfg)
	r.until("/readyz", http.StatusOK)
	waitConnected(t, w.fake)

	if _, err := w.fake.Mention("C0123456789", "U1", "<@UFAKEBOT> why is it down?"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-w.sink.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's answer never reached the turn's handler")
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after its context ended, want nil", err)
	}
	goleak.VerifyNone(t, before)
}

func waitConnected(t *testing.T, f *fakeslack.Server) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); f.Connections() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("courier never connected to Slack")
		}
	}
}

// A rejected token ends Run with ErrSlackAuth, which cmd/courier exits 1 on: no retry can mend it.
func TestRunExitsWithSlackAuthWhenTheBotTokenIsRejected(t *testing.T) {
	w := newSlackWorld(t, "wrong-bot-token") // the fake accepts only well-formed tokens
	r := start(t, w.cfg)

	err := r.wait()

	if !errors.Is(err, ErrSlackAuth) || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("Run = %v, want ErrSlackAuth naming invalid_auth", err)
	}
	if strings.Contains(err.Error(), "wrong-bot-token") {
		t.Errorf("the error holds the token: %v", err)
	}
}

func TestSlackConfigTakesItsAPIURLFromTestHooksWhenSet(t *testing.T) {
	conn := SlackConnection{}
	t.Setenv(testhooks.SlackAPIURLEnv, "http://fake-slack.test/")
	if got := slackConfig(conn, nil).APIURL; got != "http://fake-slack.test/" {
		t.Errorf("APIURL = %q in a testhooks build with %s set, want it", got, testhooks.SlackAPIURLEnv)
	}
	t.Setenv(testhooks.SlackAPIURLEnv, "")
	if got := slackConfig(conn, nil).APIURL; got != "" {
		t.Errorf("APIURL = %q with %s empty, want Slack's own (empty)", got, testhooks.SlackAPIURLEnv)
	}
}
