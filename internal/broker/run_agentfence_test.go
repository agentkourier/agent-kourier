package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// sendCounter is an agent that counts the calls that send the agent a message (a send or an answer: both are JSON-RPC
// methods that name a message), and answers every request as the stub agent does. A subscribe, a task read or a cancel is not
// a send, and is not counted.
type sendCounter struct {
	stub  *stubagent.Server
	sends atomic.Int32
	other atomic.Int32 // every other call that reached the agent
}

func newSendCounter(t *testing.T) (*sendCounter, string) {
	t.Helper()
	c := &sendCounter{stub: stubagent.NewHandler()}
	for _, name := range []string{"k8s-agent", "test-agent"} { // the agents that the Binding of the example config and that of the bindings fixture name
		c.stub.Agent("kagent", name).Next(stubagent.Text("done"))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if strings.Contains(string(body), `"method":"message/`) || strings.Contains(string(body), `"method":"SendMessage"`) || strings.Contains(string(body), `"method":"SendStreamingMessage"`) {
			c.sends.Add(1)
		} else {
			c.other.Add(1)
		}
		c.stub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(c.stub.Close)
	return c, srv.URL
}

// clusterWithJWT is cluster with a Binding token that parses as the JWT that the agent's front door would take.
func clusterWithJWT() *fake.Clientset {
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "agent-kourier-sre-alerts-oidc"}, Data: map[string][]byte{"token": []byte(jwt("a@example.test"))}}
	return fake.NewClientset(secretOf("agent-kourier-system", "slack-main", "botToken", "appToken"), token)
}

// agentConfig is electionConfig for a replica whose Binding sre/sre-alerts reaches the agent at url, and whose store holds one
// queued turn of a thread of that Binding (seedQueuedTurn).
func agentConfig(t *testing.T, client *fake.Clientset, fence *fakeFence, url string) (RunConfig, *replica) {
	t.Helper()
	c, rep := electionConfig(t, client, fence, "pod-a_1")
	seedQueuedTurn(t, c, url)
	return c, rep
}

// seedQueuedTurn points the Binding sre/sre-alerts of c at the agent at url, and leaves one turn queued in its store, as a
// crash of the leader before it sent the turn would: the first thing a leader does is send it.
func seedQueuedTurn(t *testing.T, c RunConfig, url string) {
	t.Helper()
	path := filepath.Join(c.ConfigDir, "agentbackend.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "http://kagent-controller.kagent-pilot:8083", url, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "agent-kourier-system/slack-main", Channel: "C0123456789", Thread: "1759560000.123456"}
	if err := seed.UpsertSession(t.Context(), store.Session{Key: key, Binding: "sre/sre-alerts", LastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.EnqueueReply(t.Context(), store.Reply{ID: "turn-1", Key: key, UserID: "U0123456789", MessageRef: key.Thread, Text: "what is wrong?", EnqueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
}

// The control of the test below: with a lease that is as young as renewals keep it, the queued turn is sent to the agent,
// once, and nothing stops the process.
func TestRunALeaderWhoseLeaseIsFreshSendsTheQueuedTurnToTheAgent(t *testing.T) {
	agent, url := newSendCounter(t)
	client, fence := clusterWithJWT(), &fakeFence{}
	c, rep := agentConfig(t, client, fence, url)
	now := time.Now()
	c.leaseNow = func() time.Time { return now } // time stands still, so the lease is never older than its last renewal
	rep.running = start(t, c)

	eventually(t, 10*time.Second, "the queued turn did not reach the agent", func() bool { return agent.sends.Load() == 1 })
	time.Sleep(testElection.RetryPeriod * 3)
	if n := agent.sends.Load(); n != 1 {
		t.Errorf("%d sends reached the agent; want exactly one", n)
	}
	select {
	case code := <-rep.exits:
		t.Errorf("the process stopped with exit %d; want it to lead\n%s", code, rep.logs.String())
	default:
	}
}

// A leader whose lease is past its renew deadline sends nothing to the agent. Here the lease is a minute old at every
// reading, as a leader's is that was paused as a whole and came back, and the queued turn is the first thing it does: no send
// reaches the agent, the process is stopped once, with exit 5, for the reason that an agent call was refused, and not for the loss
// of the lease, which refuses nothing. The turn is still queued, for the leader that takes over.
func TestRunALeaderPastItsRenewDeadlineSendsNothingToTheAgent(t *testing.T) {
	agent, url := newSendCounter(t)
	client, fence := clusterWithJWT(), &fakeFence{}
	c, rep := agentConfig(t, client, fence, url)
	c.leaseNow = stepping()
	rep.running = start(t, c)

	select {
	case code := <-rep.exits:
		if code != ExitLeadershipLost {
			t.Errorf("exit code = %d; want %d", code, ExitLeadershipLost)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("the process did not stop: a leader past its renew deadline sent to the agent (%d sends)\n%s", agent.sends.Load(), rep.logs.String())
	}
	time.Sleep(testElection.RetryPeriod * 3) // the runner's other attempts, if it were to make them, would be refused too
	if n := agent.sends.Load(); n != 0 {
		t.Errorf("%d sends reached the agent; want none: a refused send never leaves the process", n)
	}
	logs := rep.logs.String()
	if n := strings.Count(logs, "leadership lost"); n != 1 {
		t.Errorf("%d leadership-lost lines; want exactly one:\n%s", n, logs)
	}
	if !strings.Contains(logs, "an agent call (") || strings.Contains(logs, leaseLostReason) {
		t.Errorf("the log does not say that an agent call was refused, or says the lease was lost:\n%s", logs)
	}
	// The turn was refused, not lost: it is still the head of its thread's queue, undelivered, for the leader that takes over.
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	key := store.ConversationKey{Connection: "agent-kourier-system/slack-main", Channel: "C0123456789", Thread: "1759560000.123456"}
	if head, err := st.NextReply(t.Context(), key); err != nil || head.ID != "turn-1" {
		t.Errorf("NextReply = %q, %v; want the refused turn, still queued", head.ID, err)
	}
}
