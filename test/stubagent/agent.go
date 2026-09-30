package stubagent

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
)

// Agent is one scripted agent. Script it with Next; register it with Server.Agent.
type Agent struct {
	s    *Server
	ns   string
	name string

	// Guarded by s.mu.
	turns    [][]Step
	sessions map[string]*session
}

// Next queues a turn for the next message the agent accepts. Turns run in the order queued.
// A message that is rejected does not use one up.
func (a *Agent) Next(steps ...Step) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.turns = append(a.turns, steps)
}

// nextTurn picks the steps for a new turn. The caller holds s.mu.
func (a *Agent) nextTurn() ([]Step, bool) {
	if len(a.turns) == 0 {
		return nil, false
	}
	steps := a.turns[0]
	a.turns = a.turns[1:]
	return steps, true
}

// serveCard answers with the agent card, which lists the HITL extension as kagent's does.
func (a *Agent) serveCard(w http.ResponseWriter, r *http.Request) {
	card := obj{
		"name":        a.name,
		"description": "stubagent: a scripted stand-in for a kagent agent",
		"version":     "1.0.0",
		"supportedInterfaces": []obj{{
			"url": "http://" + r.Host + agentPath(a.ns, a.name), "protocolBinding": "JSONRPC", "protocolVersion": "1.0",
		}},
		"capabilities": obj{"streaming": true, "extensions": []obj{
			{"uri": HITLExtension, "description": "Human approval and user input"},
		}},
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"skills":             []obj{},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(card)
}

// serveRPC decodes one JSON-RPC call and dispatches it.
func (a *Agent) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	o := &rw{s: a.s, w: w}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		o.fail(&rpcError{codeParse, "parse error", "PARSE_ERROR"})
		return
	}
	o.id = req.ID
	switch req.Method {
	case "SendMessage":
		a.send(r.Context(), o, req.Params)
	default:
		o.fail(&rpcError{codeMethod, "method not found: " + req.Method, "METHOD_NOT_FOUND"})
	}
}

// send handles SendMessage: it admits the message, then follows the task to the end of the
// turn and answers with its final snapshot.
func (a *Agent) send(ctx context.Context, o *rw, params json.RawMessage) {
	var p struct {
		Message obj `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Message == nil {
		o.fail(invalidParams("params.message is required"))
		return
	}
	st, err := a.admit(p.Message)
	if err != nil {
		o.fail(err)
		return
	}
	a.s.follow(ctx, o, st)
	o.done()
}

// admit turns a message into a running task: it finds or opens the session, creates the task
// with the events kagent opens a stream with, and starts the turn.
func (a *Agent) admit(m obj) (*stream, *rpcError) {
	id, _ := m["messageId"].(string)
	if id == "" {
		return nil, invalidParams("params.message.messageId is required")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	steps, ok := a.nextTurn()
	if !ok {
		return nil, internalError("stubagent: no turn scripted for %s/%s", a.ns, a.name)
	}
	ctxID, _ := m["contextId"].(string)
	sess := a.session(ctxID)
	now := s.stamp()
	t := &task{id: s.newID("task"), sess: sess, created: now, changed: make(chan struct{})}
	in := maps.Clone(m)
	in["contextId"] = sess.id
	sub := obj{"id": t.id, "contextId": sess.id, "history": []obj{in}, "status": obj{"state": submitted}}
	t.push(obj{"task": sub}, false)
	sub = maps.Clone(sub)
	sub["metadata"] = obj{adkExtension: true}
	t.push(obj{"task": sub}, false)
	t.history = []obj{withTask(in, t.id)}
	t.setState(working, now)
	t.push(t.statusEvent(), false)
	a.launch(t, steps)
	return &stream{t: t}, nil
}

// session finds the session for a contextId, or opens one. An empty contextId gets a new id.
// UNCONFIRMED: a contextId that names no session opens one with that id.
func (a *Agent) session(ctxID string) *session {
	if ctxID == "" {
		ctxID = a.s.newID("context")
	}
	if a.sessions[ctxID] == nil {
		a.sessions[ctxID] = &session{id: ctxID}
	}
	return a.sessions[ctxID]
}

// launch plays steps for the task in the background. The caller holds s.mu.
func (a *Agent) launch(t *task, steps []Step) {
	a.s.wg.Add(1)
	go func() {
		defer a.s.wg.Done()
		r := &run{s: a.s, t: t}
		for _, st := range steps {
			if !st.play(r) {
				return
			}
		}
		r.finish(completed)
	}()
}

// stream says where a response starts in a task's log.
type stream struct {
	t    *task
	from int
}

// follow writes the task's events from st.from until one is final, the client goes away, or
// the server closes. The turn runs on regardless of the client.
func (s *Server) follow(ctx context.Context, o *rw, st *stream) {
	from := st.from
	for {
		s.mu.Lock()
		evs, changed := st.t.log[from:], st.t.changed
		s.mu.Unlock()
		for _, e := range evs {
			o.event(e.res)
			if e.final {
				return
			}
		}
		from += len(evs)
		select {
		case <-changed:
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func withTask(m obj, taskID string) obj {
	m = maps.Clone(m)
	m["taskId"] = taskID
	return m
}
