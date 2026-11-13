package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// Through a real Pool and Agent, a task left by an agent card that is too large is released by a restart once the card is fixed. The first
// Recover meets the card on the read of the task, tells the thread (the notice names a restart, since no control of the thread takes the task up),
// and leaves the task recorded; the card is fixed, a second Recover is the restart: it reads the task, which has ended, renders its end and clears
// the record, with no second notice. This is what the notice and the docs promise.
func TestRestartFollowsTheTaskLeftByACardOnceItIsFixed(t *testing.T) {
	f, sink := restarted(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	var mu sync.Mutex
	var bad = true
	setBad := func(v bool) { mu.Lock(); bad = v; mu.Unlock() }
	isBad := func() bool { mu.Lock(); defer mu.Unlock(); return bad }
	methods := map[string]int{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	card := &a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)}}
	card.Capabilities.Streaming = true
	cardOK := a2asrv.NewStaticAgentCardHandler(card)
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		if isBad() {
			_, _ = w.Write([]byte(`{"name":"ask","x":"` + strings.Repeat("y", dialect.MaxCardBytes) + `"}`))
			return
		}
		cardOK.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		mu.Lock()
		methods[call.Method]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call.Method == "GetTask" { // the task ended while nobody followed it
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"not in this test"}}`))
	})
	p := agent.NewPool(generic.New(0), agent.Config{ReconnectDelay: time.Millisecond, InactivityTimeout: time.Minute})
	t.Cleanup(p.Close)
	f.real = p.Agent(agent.Target{Binding: "ns/b1", Agent: v1alpha1.AgentRef{Name: "ask", Namespace: "ns"},
		Backend: &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: srv.URL}},
		Credentials: func(context.Context) (dialect.Credentials, error) {
			return dialect.Credentials{Token: secret.New("fake-test-token")}, nil
		}})

	f.recover(t) // the process that meets the bad card
	testutil.Eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != cardTooLargeDetail {
		t.Errorf("notice = %q; want %q", got[0].Detail, cardTooLargeDetail)
	}
	if id := f.taskID(); id != "t1" {
		t.Fatalf("task %q recorded; want t1: nothing follows it, and the agent still has it", id)
	}

	setBad(false) // the card is fixed, and the broker restarts
	f.recover(t)
	waitFinal(t, sink) // the task's end, rendered by the recovery that follows it
	testutil.Eventually(t, "the record to be cleared", func() bool { return f.taskID() == "" })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 {
		t.Errorf("failed notices = %+v; want the one: a restart that works tells the thread nothing more", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if methods["GetTask"] < 2 {
		t.Errorf("calls = %v; want the task read again after the restart", methods)
	}
}
