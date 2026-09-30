package stubagent

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
)

// Agent is one scripted agent. Script it with Next and Reply; register it with Server.Agent.
type Agent struct {
	s    *Server
	ns   string
	name string

	// Guarded by s.mu.
	turns    [][]Step
	reply    func(prompt string) []Step
	sessions map[string]*session
	tasks    map[string]*task
	messages map[string]*task // the task each accepted messageId started
}

// Next queues a turn for the next message the agent accepts. Turns run in the order queued.
// A message that is rejected does not use one up.
func (a *Agent) Next(steps ...Step) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.turns = append(a.turns, steps)
}

// Reply sets the script for messages that find no queued turn: f gets the message's text and
// returns the steps of its turn. It suits an agent whose answer depends on the prompt.
func (a *Agent) Reply(f func(prompt string) []Step) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.reply = f
}

// nextTurn picks the steps for a new turn. The caller holds s.mu.
func (a *Agent) nextTurn(prompt string) ([]Step, bool) {
	if len(a.turns) > 0 {
		steps := a.turns[0]
		a.turns = a.turns[1:]
		return steps, true
	}
	if a.reply != nil {
		return a.reply(prompt), true
	}
	return nil, false
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
	o := &rw{s: a.s, w: w, ext: strings.Contains(r.Header.Get("A2A-Extensions"), HITLExtension)}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		o.fail(&rpcError{codeParse, "parse error", "PARSE_ERROR"})
		return
	}
	o.id = req.ID
	switch req.Method {
	case "SendMessage":
		a.send(r.Context(), o, req.Params)
	case "SendStreamingMessage":
		o.stream = true
		a.send(r.Context(), o, req.Params)
	case "GetTask":
		a.getTask(o, req.Params)
	case "CancelTask":
		a.cancelTask(o, req.Params)
	case "SubscribeToTask":
		o.stream = true
		a.subscribeTask(r.Context(), o, req.Params)
	default:
		o.fail(&rpcError{codeMethod, "method not found: " + req.Method, "METHOD_NOT_FOUND"})
	}
}

// send handles SendMessage and SendStreamingMessage: it admits the message, then follows the
// task to the end of the turn, streaming its events or answering with its final snapshot.
func (a *Agent) send(ctx context.Context, o *rw, params json.RawMessage) {
	var p struct {
		Message obj `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Message == nil {
		o.fail(invalidParams("params.message is required"))
		return
	}
	st, err := a.admit(p.Message) //nolint:contextcheck // the turn outlives the request; the server's context bounds it
	if err != nil {
		o.fail(err)
		return
	}
	a.s.follow(ctx, o, st)
	o.done()
}

// admit turns a message into a running task: it finds or opens the session, creates the task
// with the events kagent opens a stream with, and starts the turn.
//
// A message whose messageId was accepted before is a replay: it joins that task, and runs no
// turn (WP-02, measured). A message that is rejected is not remembered, so it can be resent.
func (a *Agent) admit(m obj) (*stream, *rpcError) {
	id, _ := m["messageId"].(string)
	if id == "" {
		return nil, invalidParams("params.message.messageId is required")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior := a.messages[id]; prior != nil {
		return prior.join(), nil
	}
	ctxID, _ := m["contextId"].(string)
	sess := a.session(ctxID)
	if a.busy(sess) {
		return nil, &rpcError{codeBusy, "resource conflict", "UNSUPPORTED_OPERATION"}
	}
	steps, ok := a.nextTurn(promptText(m))
	if !ok {
		return nil, internalError("stubagent: no turn scripted for %s/%s", a.ns, a.name)
	}
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
	a.tasks[t.id], a.messages[id] = t, t
	a.launch(t, steps)
	return &stream{t: t}, nil
}

// busy reports whether a task of the session is working. kagent allows one at a time.
func (a *Agent) busy(sess *session) bool {
	for _, t := range a.tasks {
		if t.sess == sess && t.state == working {
			return true
		}
	}
	return false
}

// find returns the task that params name. The caller holds s.mu.
func (a *Agent) find(params json.RawMessage) (*task, *rpcError) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return nil, invalidParams("params.id is required")
	}
	if a.tasks[p.ID] == nil {
		return nil, &rpcError{codeTaskNotFound, "task not found", "TASK_NOT_FOUND"}
	}
	return a.tasks[p.ID], nil
}

// getTask answers with the task as it is now, its text so far included.
func (a *Agent) getTask(o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params)
	var snap obj
	if err == nil {
		snap = t.snapshot()
	}
	a.s.mu.Unlock()
	if err != nil {
		o.fail(err)
		return
	}
	o.event(snap)
	o.done()
}

// cancelTask stops the task's turn and ends its streams with the CANCELED task.
// UNCONFIRMED: cancelling a task that is already completed or cancelled answers -32002.
func (a *Agent) cancelTask(o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params)
	if err == nil && (t.state == completed || t.state == canceled) {
		err = &rpcError{codeNotCancelable, "task is already " + t.state, "TASK_NOT_CANCELABLE"}
	}
	var snap obj
	if err == nil {
		t.stop()
		t.setState(canceled, a.s.stamp())
		snap = t.snapshot()
		t.push(obj{"task": snap}, true)
	}
	a.s.mu.Unlock()
	if err != nil {
		o.fail(err)
		return
	}
	o.event(snap)
	o.done()
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
	ctx, stop := context.WithCancel(a.s.ctx)
	t.stop = stop
	a.s.wg.Add(1)
	go func() {
		defer a.s.wg.Done()
		defer stop()
		r := &run{s: a.s, t: t, ctx: ctx}
		for _, st := range steps {
			if !st.play(r) {
				return
			}
		}
		r.finish(completed)
	}()
}

// subscribeTask streams a task that is already running: a snapshot with the text so far, then
// what happens next. For a task that is not running, the snapshot is the whole answer.
func (a *Agent) subscribeTask(ctx context.Context, o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params)
	var st *stream
	if err == nil {
		st = t.join()
	}
	a.s.mu.Unlock()
	if err != nil {
		o.fail(err)
		return
	}
	a.s.follow(ctx, o, st)
}

// stream says where a response starts. A live join starts with a snapshot of the task, then
// the log from the index it had when the snapshot was taken, so nothing is missed or repeated.
type stream struct {
	t     *task
	first obj  // the snapshot event that opens a join, or nil
	from  int  // the first log index to send after it
	end   bool // the snapshot is the whole answer
}

// follow writes the stream until a final event, the client going away, or the server closing.
// The turn runs on regardless of the client.
func (s *Server) follow(ctx context.Context, o *rw, st *stream) {
	if st.first != nil {
		o.event(st.first)
		if st.end {
			return
		}
	}
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

// promptText joins the text parts of a message.
func promptText(m obj) string {
	var sb strings.Builder
	parts, _ := m["parts"].([]any)
	for _, p := range parts {
		if part, ok := p.(obj); ok {
			s, _ := part["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}
