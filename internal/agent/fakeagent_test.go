package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// rpc is one JSON-RPC request the fake agent received.
type rpc struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Done   <-chan struct{} `json:"-"` // closed when the client hangs up
	Header http.Header     `json:"-"`
}

// fakeAgent is an A2A server that answers each JSON-RPC call with whatever serve writes,
// usually a WP-02 fixture, and records the calls. A GET is the card fetch, the call "card".
type fakeAgent struct {
	*httptest.Server
	mu          sync.Mutex
	calls       []rpc
	noStreaming bool
	cardGate    chan struct{}
	cardStatus  int  // when set, the card fetch fails with this HTTP status
	v03         bool // the card is an A2A 0.3 card, and the agent speaks 0.3 (message/stream), as kagent 0.9.x does
}

func newFakeAgent(t *testing.T, serve func(call rpc, w http.ResponseWriter)) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{}
	fa.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		call := rpc{Method: "card"}
		if r.Method == http.MethodPost && err == nil {
			err = json.Unmarshal(body, &call)
		}
		if err != nil {
			t.Errorf("fake agent: bad request: %v", err) // not Fatal: this runs on the server's goroutine
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		call.Done, call.Header = r.Context().Done(), r.Header
		fa.mu.Lock()
		fa.calls = append(fa.calls, call)
		fa.mu.Unlock()
		if call.Method != "card" {
			serve(call, w)
			return
		}
		if fa.cardGate != nil { // a test that sets a gate closes it to release the card
			select {
			case <-fa.cardGate:
			case <-call.Done:
				return
			}
		}
		if fa.cardStatus != 0 { // the front door refuses the card fetch
			http.Error(w, "refused", fa.cardStatus)
			return
		}
		if fa.v03 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fa.v03CardJSON())
			return
		}
		card := a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(fa.URL, a2a.TransportProtocolJSONRPC)}}
		card.Capabilities.Streaming = !fa.noStreaming
		_ = json.NewEncoder(w).Encode(card)
	}))
	t.Cleanup(fa.Close)
	return fa
}

// count is how many calls of method the agent has received.
func (fa *fakeAgent) count(method string) int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	n := 0
	for _, c := range fa.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// callAt returns the i-th call the agent received.
func (fa *fakeAgent) callAt(i int) rpc {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.calls[i]
}

// agent returns an Agent that talks to fa over a plain JSON-RPC client. With noStreaming set,
// the client's card says the agent does not stream, so a2a-go sends a unary SendMessage.
func (fa *fakeAgent) agent(t *testing.T) *Agent { return fa.agentWith(t, Config{}) }

func (fa *fakeAgent) agentWith(t *testing.T, cfg Config) *Agent {
	t.Helper()
	card := &a2a.AgentCard{SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(fa.URL, a2a.TransportProtocolJSONRPC)}}
	card.Capabilities.Streaming = !fa.noStreaming
	client, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(fa.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return New(func(context.Context) (Conn, error) { return client, nil }, cfg)
}

// agentVia returns an Agent that connects the way production does: a Pool of d fetches fa's card and
// builds the client from it, so the dialect's own handling of the card and of the protocol version is in the path.
func (fa *fakeAgent) agentVia(t *testing.T, d dialect.Dialect) *Agent {
	t.Helper()
	p := NewPool(d, Config{})
	t.Cleanup(p.Close)
	return p.Agent(Target{
		Binding:     "team/sre",
		Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: fa.URL}},
		Agent:       v1alpha1.AgentRef{Name: "ask", Namespace: "kagent"},
		Credentials: token("t"),
	})
}

// v03CardJSON is the card kagent 0.9.x serves: A2A 0.3, so one url and a preferredTransport and no supportedInterfaces.
func (fa *fakeAgent) v03CardJSON() string {
	return fmt.Sprintf(`{"name": "sre_investigator", "description": "A read-only SRE assistant", "url": %q, "preferredTransport": "JSONRPC",
  "version": "", "capabilities": {"streaming": true}, "defaultInputModes": ["text"], "defaultOutputModes": ["text"],
  "skills": [{"id": "investigate", "name": "Investigate", "description": "reads the cluster", "tags": ["sre"]}]}`, fa.URL+"/")
}

// sseEvents returns the events of a recorded SSE stream, each ready to write.
func sseEvents(t *testing.T, name string) [][]byte {
	t.Helper()
	body, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Errorf("read fixture: %v", err) // not Fatal: this runs on the server's goroutine
		return nil
	}
	var evs [][]byte
	for _, ev := range bytes.SplitAfter(bytes.TrimSpace(body), []byte("\n\n")) {
		evs = append(evs, append(bytes.TrimSpace(ev), '\n', '\n'))
	}
	return evs
}

// write writes one event to a stream and flushes, so the client sees it now.
func write(w http.ResponseWriter, ev []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write(ev)
	w.(http.Flusher).Flush()
}

// sse replays the first n events of a recorded SSE stream (all of them when n < 0).
func sse(t *testing.T, w http.ResponseWriter, name string, n int) {
	t.Helper()
	for i, ev := range sseEvents(t, name) {
		if n >= 0 && i >= n {
			return
		}
		write(w, ev)
	}
}

// reply answers a unary call with a recorded JSON response.
func reply(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	body, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Errorf("read fixture: %v", err) // not Fatal: this runs on the server's goroutine
		return
	}
	_, _ = w.Write(body)
}

// drop ends the response without finishing it, as a reset connection does.
func drop() { panic(http.ErrAbortHandler) }

// message is what a caller sends; its ID must reach the agent unchanged.
func message(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

// sentMessageIDs returns the messageId of each send the agent received.
func (fa *fakeAgent) sentMessageIDs(t *testing.T) []string {
	t.Helper()
	fa.mu.Lock()
	defer fa.mu.Unlock()
	var ids []string
	for _, c := range fa.calls {
		if !strings.HasPrefix(c.Method, "Send") {
			continue
		}
		var p struct {
			Message struct {
				ID string `json:"messageId"`
			} `json:"message"`
		}
		if err := json.Unmarshal(c.Params, &p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.Message.ID)
	}
	return ids
}
