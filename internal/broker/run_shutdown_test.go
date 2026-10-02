package broker

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// slackCluster is a cluster whose connection Secret holds botToken and whose Binding token is one the
// stub agent accepts.
func slackCluster(botToken string) *fake.Clientset {
	conn := secretOf("courier-system", "slack-main", "appToken")
	conn.Data["botToken"] = []byte(botToken)
	conn.Data["appToken"] = []byte("xapp-fake-app")
	oidc := secretOf("sre", "courier-sre-alerts-oidc", "token")
	oidc.Data["token"] = []byte(jwt("courier@example.test"))
	return fake.NewClientset(conn, oidc, secretOf("sre", "courier-webhook-token", "token"))
}

// stepTrace records the shutdown steps of a Run in the order they began.
type stepTrace struct {
	mu    sync.Mutex
	steps []string
	hook  func(step string) // called as each step begins, before it runs
}

func (s *stepTrace) record(step string) {
	s.mu.Lock()
	s.steps = append(s.steps, step)
	s.mu.Unlock()
	if s.hook != nil {
		s.hook(step)
	}
}

func (s *stepTrace) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.steps, ",")
}

// slowStop is a Slack that takes its time to stop, and notes when it has.
type slowStop struct {
	idleSlack
	started sync.WaitGroup // Done once Run has begun, which is when startup is over
	stopped atomic.Int64   // the UnixNano at which Run returned
}

func (s *slowStop) Run(ctx context.Context, _ slack.Sink) error {
	s.started.Done()
	<-ctx.Done()
	time.Sleep(150 * time.Millisecond)
	s.stopped.Store(time.Now().UnixNano())
	return ctx.Err()
}

// Run stops what feeds a part before the part: the Slack run finishes, and is waited for, before the
// manager is closed, and every step runs in the agreed order.
func TestRunStopsTheSlackRunAndWaitsForItBeforeTheManager(t *testing.T) {
	slow := &slowStop{}
	slow.started.Add(1)
	var managerAt atomic.Int64
	trace := &stepTrace{hook: func(step string) {
		if step == "manager" {
			managerAt.Store(time.Now().UnixNano())
		}
	}}
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return slow }
	c.shutdownTrace = trace.record
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	slow.started.Wait() // readiness comes before the Slack run does: stop only a Run that has finished starting

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}

	if want := "intake,interactions,manager,drain,pool,resolver,store,health"; trace.got() != want {
		t.Errorf("Run shut down as %s, want %s", trace.got(), want)
	}
	if stopped := slow.stopped.Load(); stopped == 0 || managerAt.Load() < stopped {
		t.Errorf("the manager began closing at %d, before the Slack run returned at %d: the intake was not waited for", managerAt.Load(), stopped)
	}
}

// ctxSlack is an idle Slack that keeps the context its Run was given.
type ctxSlack struct {
	idleSlack
	got chan context.Context // receives the context when Run begins
}

func (s *ctxSlack) Run(ctx context.Context, _ slack.Sink) error {
	s.got <- ctx
	<-ctx.Done()
	return ctx.Err()
}

// A signal after startup ends Run's own context and nothing else: each part sees its context end at its
// own shutdown step. Here the Slack run must still be live when the intake step begins, and that is
// after the signal, so the test gives a context that a signal cancelled by mistake time to show it.
func TestRunKeepsThePartsRunningUntilTheirShutdownStepAfterASignal(t *testing.T) {
	slk := &ctxSlack{got: make(chan context.Context, 1)}
	var slackCtx context.Context // set before the signal, read after it
	var liveAtIntake, ended atomic.Bool
	trace := &stepTrace{hook: func(step string) {
		if step != "intake" {
			return
		}
		time.Sleep(200 * time.Millisecond) // a cancellation that the signal sent in error arrives within this
		liveAtIntake.Store(slackCtx.Err() == nil)
		ended.Store(true)
	}}
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return slk }
	c.shutdownTrace = trace.record
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	slackCtx = <-slk.got // the Slack run has begun, so startup is over

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}

	if !ended.Load() {
		t.Fatal("the intake step never began")
	}
	if !liveAtIntake.Load() {
		t.Error("the Slack run's context had ended by the time the intake step began: the signal cancelled the parts, instead of the shutdown stopping them in order")
	}
}

// connSlack is an idle Slack that makes one request with the HTTP client Run gave it, as the real
// client does, so that its connection is left idle in the transport that Run closes at shutdown.
type connSlack struct {
	idleSlack
	client  *http.Client
	url     string
	started chan struct{} // closed once the request is done
}

func (s *connSlack) Run(ctx context.Context, _ slack.Sink) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body) // read to the end, so that the connection goes back to the pool
	_ = resp.Body.Close()
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

// The Slack client's idle connections are closed after the final drain, which posts through that
// client, and not at the intake step: closed earlier, the drain's own calls would open connections that
// nothing closes. The connection is still open when the drain begins, and is closed by the time Run returns.
func TestRunClosesTheSlackConnectionsAfterTheFinalDrain(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	slk := &connSlack{url: srv.URL, started: make(chan struct{})}
	var openAtDrain atomic.Int32
	openAtDrain.Store(-1)
	trace := &stepTrace{hook: func(step string) {
		if step == "drain" {
			time.Sleep(200 * time.Millisecond) // a connection closed too early is seen as closed within this
			openAtDrain.Store(closed.Load())
		}
	}}
	c := startConfig(t)
	c.newSlack = func(sc slack.Config) slackAPI { slk.client = sc.HTTPClient; return slk }
	c.shutdownTrace = trace.record
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	<-slk.started

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	if n := openAtDrain.Load(); n != 0 {
		t.Errorf("%d connections of the Slack client were closed when the drain began, want none: they close after it", n)
	}
	for deadline := time.Now().Add(2 * time.Second); closed.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Slack client's idle connection was never closed")
		}
	}
}

// rpcLog wraps a stub agent and notes the A2A methods it is asked.
type rpcLog struct {
	next    http.Handler
	mu      sync.Mutex
	methods []string
}

func (l *rpcLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	for _, m := range []string{"GetTask", "SendStreamingMessage", "ListTasks"} {
		if bytes.Contains(body, []byte(`"method":"`+m+`"`)) {
			l.mu.Lock()
			l.methods = append(l.methods, m)
			l.mu.Unlock()
		}
	}
	l.next.ServeHTTP(w, r)
}

func (l *rpcLog) saw(method string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.methods {
		if m == method {
			return true
		}
	}
	return false
}

// A task an earlier run left recorded is asked after when Run starts: Recover is called, and finds the
// agent, so the task is reconciled instead of forgotten.
func TestRunRecoversTheSessionsAnEarlierRunLeft(t *testing.T) {
	stub := stubagent.NewHandler()
	t.Cleanup(stub.Close)
	stub.Agent("kagent", "k8s-agent")
	log := &rpcLog{next: stub}
	srv := httptest.NewServer(log)
	t.Cleanup(srv.Close)

	c := startConfig(t)
	c.Client = slackCluster("xoxb-fake-bot")
	be := filepath.Join(c.ConfigDir, "agentbackend.yaml")
	raw, err := os.ReadFile(be)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(be, []byte(strings.Replace(string(raw), "http://kagent-controller.kagent-pilot:8083", srv.URL, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "courier-system/slack-main", Channel: "C0123456789", ThreadTS: "1.1"}
	if err := seed.UpsertSession(t.Context(), store.Session{Key: key, Binding: "sre/sre-alerts", ContextID: "ctx-1", TaskID: "task-1", LastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	for deadline := time.Now().Add(10 * time.Second); !log.saw("GetTask"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the agent was never asked about the recorded task: Recover did not run")
		}
	}
}

// What is still unposted when the shutdown reaches the outbox is posted by its final drain. The row
// appears as the drain step begins, as one enqueued in the last moment would, and Slack sees it.
func TestRunPostsWhatIsPendingInAFinalDrain(t *testing.T) {
	c := startConfig(t)
	posted := make(chan string, 4)
	rec := &recordingSlack{posted: posted, started: make(chan struct{})}
	c.newSlack = func(slack.Config) slackAPI { return rec }
	trace := &stepTrace{hook: func(step string) {
		if step != "drain" {
			return
		}
		st, err := sqlite.Open(context.Background(), c.DBPath)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = st.Close() }()
		post := outbox.Post{Source: "late", Role: store.OutboxNotice, Channel: "C0123456789", Out: chat.Say(chat.Message{Fallback: "last words"})}
		if _, err := outbox.New(outbox.Config{Store: st, Platform: slack.NewPlatform(idleSlack{}, "ns/conn")}).Enqueue(context.Background(), post); err != nil {
			t.Error(err)
		}
	}}
	c.shutdownTrace = trace.record
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	<-rec.started // readiness comes before the Slack run does: stop only a Run that has finished starting

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	select {
	case text := <-posted:
		if text != "last words" {
			t.Errorf("Slack was sent %q, want the pending post", text)
		}
	default:
		t.Error("the post that was pending at shutdown never reached Slack: there was no final drain")
	}
}

// recordingSlack is an idle Slack that records the text of each post it takes.
type recordingSlack struct {
	idleSlack
	posted  chan string
	started chan struct{} // closed when Run begins
	once    sync.Once
}

func (s *recordingSlack) Run(ctx context.Context, _ slack.Sink) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (s *recordingSlack) Post(_ context.Context, _, _ string, c slack.Content, _ string) (string, error) {
	s.posted <- c.Text
	return "1.0", nil
}
