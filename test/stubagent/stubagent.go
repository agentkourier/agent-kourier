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
// Reply, ReplyWithHistory, and the then of Ask and RequestApproval, run outside the server's
// lock and may call the Server. They may then run for a message that is rejected after they
// return, because another send got in first.
//
// Behavior that no fixture shows is guessed, and its comment carries the tag UNCONFIRMED:.
// Those items are the agent card's fields other than the in-cluster URL and the extension, the
// 404 for an unknown route, a card that needs no token, the SendMessage result (a task under
// result.task) and its ignoring of params.configuration, a contextId that names no session
// opening one, the errors for cancelling or replying to a task that has ended (-32002 and
// -32004), a reply to a task that is not waiting, several tools in one approval pause, a new
// task in a session with a paused one, a replay of an answer (refused, not joined, because it
// carries a contextId), and the 401 of WithRequiredAuth. Identity adds more:
// only a send and GetTask were measured refused for another identity, so the refusal of a
// HITL reply, a replay, SubscribeToTask and CancelTask is a guess; so are a sub-only token
// owning its session by the sub, the owner check running before the busy and wedged checks,
// the 401 for an Authorization header that is not a well-formed Bearer credential with an email
// or a sub (only an absent header is anonymous), and an anonymous owner, which
// matches no real kagent mode. ListTasks was measured (WP-03) for a list by contextId, with no
// filter and refused, all within one page; what is guessed is paging past a page, the errors
// for a bad pageSize or page token, the order by creation, an unknown contextId listing
// nothing, the filters it ignores, and every read of a pause, other than a list, showing it as it
// was made, whatever the reader's header (see pause.message). The envelope errors (-32600,
// -32601, -32602, -32700) follow a2a-go v2.6.0, not kagent, which vendors a2a-go v2.5.0.
//
// Command test/stubagent/cmd/stubagent serves the agents of SpikeAgents, so that the WP-02
// spike can run against the stub, and of ScenarioAgents, which the E2E scenarios drive.
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
	idTag       string // in every id the stub makes, if not empty

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

// WithIDTag puts tag in every id the stub makes ("task-<tag>-7"), so that the ids of one run of the
// stub are not those of another. The counter behind the ids starts again at 1 when the stub restarts,
// while a client keeps what it stored of the earlier run (courier derives the ID of an interaction from
// the task and request ids, and reads a stored one as already decided), as it would not of an agent
// whose ids are UUIDs. An empty tag, the default, keeps the plain "task-7", for tests that count.
func WithIDTag(tag string) Option { return func(s *Server) { s.idTag = tag } }

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

// unlocked runs f with s.mu released, and takes it again even if f panics, so that the caller's
// deferred Unlock stays balanced and the panic only fails the request it happens in. The caller
// holds s.mu.
func (s *Server) unlocked(f func()) {
	s.mu.Unlock()
	defer s.mu.Lock()
	f()
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

func (s *Server) newID(kind string) string {
	if s.idTag == "" {
		return fmt.Sprintf("%s-%d", kind, s.seq.Add(1))
	}
	return fmt.Sprintf("%s-%s-%d", kind, s.idTag, s.seq.Add(1))
}

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
	owner, authorized := identify(r, s.requireAuth)
	switch {
	case !authorized:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case a == nil:
		http.NotFound(w, r)
	case len(parts) == 4 && parts[3] == cardFile && r.Method == http.MethodGet:
		a.serveCard(w)
	case len(parts) == 3 && r.Method == http.MethodPost:
		a.serveRPC(w, r, owner)
	default:
		http.NotFound(w, r)
	}
}
