package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// rpc is one JSON-RPC request the fake agent received.
type rpc struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Done   <-chan struct{} `json:"-"` // closed when the client hangs up
}

// fakeAgent is an A2A server that answers each JSON-RPC call with whatever serve writes,
// usually a fixture recorded in WP-02, and remembers the calls it got.
type fakeAgent struct {
	*httptest.Server
	mu    sync.Mutex
	calls []rpc
}

func newFakeAgent(t *testing.T, serve func(call rpc, w http.ResponseWriter)) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{}
	fa.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call rpc
		if err := json.Unmarshal(body, &call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		call.Done = r.Context().Done()
		fa.mu.Lock()
		fa.calls = append(fa.calls, call)
		fa.mu.Unlock()
		serve(call, w)
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

// agent returns an Agent that talks to fa over a plain JSON-RPC client.
func (fa *fakeAgent) agent(t *testing.T) *Agent { return fa.agentWith(t, Config{}) }

func (fa *fakeAgent) agentWith(t *testing.T, cfg Config) *Agent {
	t.Helper()
	card := &a2a.AgentCard{SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(fa.URL, a2a.TransportProtocolJSONRPC)}}
	card.Capabilities.Streaming = true
	client, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(fa.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return New(func(context.Context) (Conn, error) { return client, nil }, cfg)
}

// sseEvents returns the events of a recorded SSE stream, each ready to write.
func sseEvents(t *testing.T, name string) [][]byte {
	t.Helper()
	body, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
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
