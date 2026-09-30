// Package stubagent is an in-process A2A server that behaves like kagent 1.0.0-alpha5, for
// courier's unit and fast-tier tests (spec 12.1). It speaks A2A 1.0 JSON-RPC at kagent's
// paths: POST /agents/{ns}/{name}, with the card at
// /agents/{ns}/{name}/.well-known/agent-card.json.
//
// What it reproduces was measured against kagent by the WP-02 spike; the recorded exchanges
// are in test/fixtures/kagent/. Tests do not replay them. Each test scripts the agent's next
// turns instead, with Agent.Next or Agent.Reply and the steps Text, Tool, Hold, Ask and
// RequestApproval, for example agent.Next(stubagent.Text("tick 1 ", "tick 2 ")). The tests of
// this package compare what the stub sends with the fixtures, event by event.
//
// The then of Ask and RequestApproval runs under the server's lock, so it must not call the
// Server. Reply runs outside it and may, but it may then run for a message that is rejected
// after it returns, because another send got in first.
//
// Behavior that no fixture shows is guessed, and its comment carries the tag UNCONFIRMED:.
// Those items are the agent card's fields other than the in-cluster URL and the extension, the
// 404 for an unknown route, a card that needs no token, the SendMessage result (a task under
// result.task) and its ignoring of params.configuration, a contextId that names no session
// opening one, the errors for cancelling or replying to a task that has ended (-32002 and
// -32004), a reply to a task that is not waiting, several tools in one approval pause, a new
// task in a session with a paused one, and the 401 of WithRequiredAuth. The envelope errors
// (-32600, -32601, -32602, -32700) follow a2a-go v2.6.0, not kagent, which vendors a2a-go
// v2.5.0.
//
// Command test/stubagent/cmd/stubagent serves the agents of SpikeAgents so that the WP-02
// spike can run against the stub.
package stubagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HITLExtension is the URI of kagent's human-in-the-loop extension.
const HITLExtension = "https://kagent.dev/extensions/hitl/v1"

// Server serves any number of stub agents. It is an http.Handler and is safe for concurrent
// use.
type Server struct {
	srv         *httptest.Server // nil when used only as a handler
	now         func() time.Time
	tick        time.Duration
	requireAuth bool
	ctx         context.Context // ends the running turns
	stop        context.CancelFunc
	wg          sync.WaitGroup // running turns
	seq         atomic.Int64

	mu     sync.Mutex
	agents map[string]*Agent        // by "ns/name", guarded by mu
	gates  map[string]chan struct{} // closed once released, guarded by mu
}

// Option configures a Server.
type Option func(*Server)

// WithClock sets the clock that timestamps come from. The default is time.Now.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithTick sets the pause between the text chunks of a turn. The default is none, so turns
// run as fast as the client reads.
func WithTick(d time.Duration) Option { return func(s *Server) { s.tick = d } }

// NewHandler returns a Server for use as an http.Handler, with no listener of its own. Call
// Close when done.
func NewHandler(opts ...Option) *Server {
	s := &Server{now: time.Now, agents: map[string]*Agent{}, gates: map[string]chan struct{}{}}
	s.ctx, s.stop = context.WithCancel(context.Background())
	for _, o := range opts {
		o(s)
	}
	return s
}

// New starts a stub on a local port. Call Close when done.
func New(opts ...Option) *Server {
	s := NewHandler(opts...)
	s.srv = httptest.NewServer(s)
	return s
}

// URL returns the base URL of a Server started with New, without a trailing slash. It is
// empty for a Server from NewHandler.
func (s *Server) URL() string {
	if s.srv == nil {
		return ""
	}
	return s.srv.URL
}

// AgentURL returns the endpoint of an agent, the URL a client posts JSON-RPC to.
func (s *Server) AgentURL(ns, name string) string { return s.URL() + agentPath(ns, name) }

func agentPath(ns, name string) string { return "/agents/" + ns + "/" + name }

// Close stops the running turns and shuts down a Server started with New.
func (s *Server) Close() {
	s.stop()
	if s.srv != nil {
		s.srv.Close()
	}
	s.wg.Wait()
}

// WaitTurn waits for the turn last played for the task to end: it completed, paused for input,
// or was cancelled. A test uses it to know that a turn has said all it will, whatever a
// client has read. It returns at once if the turn is over, and an error if the task is unknown
// or ctx ends first.
func (s *Server) WaitTurn(ctx context.Context, taskID string) error {
	var done chan struct{}
	s.mu.Lock()
	for _, a := range s.agents {
		if t := a.tasks[taskID]; t != nil {
			done = t.done
		}
	}
	s.mu.Unlock()
	if done == nil {
		return fmt.Errorf("stubagent: no task %q", taskID)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the turn of task %s: %w", taskID, ctx.Err())
	}
}

// Release opens the gate that Hold steps wait at. A gate stays open, so releasing it before a
// turn gets there lets that turn straight through.
func (s *Server) Release(gate string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.gateLocked(gate):
	default:
		close(s.gateLocked(gate))
	}
}

// gateLocked returns the gate's channel, creating it. The caller holds mu.
func (s *Server) gateLocked(gate string) chan struct{} {
	if s.gates[gate] == nil {
		s.gates[gate] = make(chan struct{})
	}
	return s.gates[gate]
}

func (s *Server) newID(kind string) string { return fmt.Sprintf("%s-%d", kind, s.seq.Add(1)) }

func (s *Server) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

// Agent returns the agent ns/name, registering it on first use. An unregistered agent's
// endpoint and card answer 404.
func (s *Server) Agent(ns, name string) *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ns + "/" + name
	if s.agents[key] == nil {
		s.agents[key] = &Agent{s: s, ns: ns, name: name,
			sessions: map[string]*session{}, tasks: map[string]*task{}, messages: map[string]*task{}}
	}
	return s.agents[key]
}

const cardFile = ".well-known/agent-card.json"

// ServeHTTP routes a request to an agent's card or JSON-RPC endpoint.
// UNCONFIRMED: an unknown agent or route answers 404, and a card needs no token unless
// WithRequiredAuth is set.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 4)
	if len(parts) < 3 || parts[0] != "agents" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	a := s.agents[parts[1]+"/"+parts[2]]
	s.mu.Unlock()
	switch _, ok := bearer(r); {
	case s.requireAuth && !ok:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case a == nil:
		http.NotFound(w, r)
	case len(parts) == 4 && parts[3] == cardFile && r.Method == http.MethodGet:
		a.serveCard(w)
	case len(parts) == 3 && r.Method == http.MethodPost:
		a.serveRPC(w, r)
	default:
		http.NotFound(w, r)
	}
}
