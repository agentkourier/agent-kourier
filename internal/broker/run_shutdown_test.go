package broker

import (
	"bytes"
	"context"
	"io"
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

	if want := "intake,manager,drain,pool,resolver,store,health"; trace.got() != want {
		t.Errorf("Run shut down as %s, want %s", trace.got(), want)
	}
	if stopped := slow.stopped.Load(); stopped == 0 || managerAt.Load() < stopped {
		t.Errorf("the manager began closing at %d, before the Slack run returned at %d: the intake was not waited for", managerAt.Load(), stopped)
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
		post := outbox.Post{Source: "late", Role: store.OutboxNotice, Channel: "C0123456789", Content: slack.Content{Text: "last words"}}
		if _, err := outbox.New(outbox.Config{Store: st, Poster: idleSlack{}}).Enqueue(context.Background(), post); err != nil {
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
