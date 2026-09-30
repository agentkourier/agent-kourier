package stubagent

import (
	"context"
	"encoding/json"
	"errors"
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
// returns the steps of its turn. It suits an agent whose answer depends on the prompt. f runs
// under the server's lock and must not call the Server.
func (a *Agent) Reply(f func(prompt string) []Step) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.reply = f
}

// pick takes the next turn: a queued one, else the Reply function, which the caller runs
// outside the lock. The caller holds s.mu.
func (a *Agent) pick() (steps []Step, reply func(string) []Step, ok bool) {
	if len(a.turns) > 0 {
		steps = a.turns[0]
		a.turns = a.turns[1:]
		return steps, nil, true
	}
	return nil, a.reply, a.reply != nil
}

// kagentInCluster is the address kagent's card advertises for its agents (WP-02 section 8,
// measured). A client outside the cluster cannot use it and must override it, and the stub
// advertises it too, so that a client which stopped overriding fails against the stub.
const kagentInCluster = "http://kagent-controller.kagent.svc:8083"

// serveCard answers with the agent card. Measured (WP-02): the in-cluster URL and the HITL
// extension in capabilities.extensions. UNCONFIRMED: every other field, which the stub fills in
// with plausible A2A 1.0 values (the name, description, version, protocol version, streaming,
// input and output modes, and an empty skills list).
func (a *Agent) serveCard(w http.ResponseWriter) {
	card := obj{
		"name":        a.name,
		"description": "stubagent: a scripted stand-in for a kagent agent",
		"version":     "1.0.0",
		"supportedInterfaces": []obj{{
			"url": kagentInCluster + agentPath(a.ns, a.name), "protocolBinding": "JSONRPC", "protocolVersion": "1.0",
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
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	o := &rw{s: a.s, w: w, owner: ownerOf(r), ext: strings.Contains(r.Header.Get("A2A-Extensions"), HITLExtension)}
	// The envelope checks and their messages follow a2a-go v2.6.0 a2asrv/jsonrpc.go ServeHTTP
	// and handleUnmarshalError.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			o.fail(&rpcError{codeParams, "invalid params: " + err.Error(), "INVALID_PARAMS"})
		} else {
			o.fail(&rpcError{codeParse, "parse error: " + err.Error(), "PARSE_ERROR"})
		}
		return
	}
	if !validID(req.ID) {
		o.fail(invalidRequest())
		return
	}
	o.id = req.ID
	if req.JSONRPC != "2.0" || req.Method == "" {
		o.fail(invalidRequest())
		return
	}
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
		o.fail(&rpcError{codeMethod, "method not found", "METHOD_NOT_FOUND"})
	}
}

// send handles SendMessage and SendStreamingMessage: it admits the message, then follows the
// task to the end of the turn, streaming its events or answering with its final snapshot.
// UNCONFIRMED: SendMessage blocks until the turn ends and ignores params.configuration
// (a blocking option, history length, push notification config).
func (a *Agent) send(ctx context.Context, o *rw, params json.RawMessage) {
	var p struct {
		Message obj `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Message == nil {
		o.fail(invalidParams("message is required"))
		return
	}
	st, err := a.admit(p.Message, o.owner) //nolint:contextcheck // the turn outlives the request; the server's context bounds it
	if err != nil {
		o.fail(err)
		return
	}
	if !a.s.follow(ctx, o, st) {
		o.abort()
		return
	}
	o.done()
}

// validateMessage rejects what a2a-go v2.6.0 rejects with -32602: a2asrv/handler.go
// handleSendMessage, for a message with no ID, no parts or no role, and a2a/core.go
// Part.UnmarshalJSON, for a part that does not have exactly one of text, raw, data and url.
func validateMessage(m obj) *rpcError {
	if id, _ := m["messageId"].(string); id == "" {
		return invalidParams("message ID is required")
	}
	parts, _ := m["parts"].([]any)
	if len(parts) == 0 {
		return invalidParams("message parts is required")
	}
	if role, _ := m["role"].(string); role == "" {
		return invalidParams("message role is required")
	}
	for _, p := range parts {
		part, _ := p.(obj)
		n := 0
		for _, kind := range []string{"text", "raw", "data", "url"} {
			if _, ok := part[kind]; ok {
				n++
			}
		}
		if n != 1 {
			return invalidParams("expected exactly one of text, raw, data, or url, got %d", n)
		}
	}
	return nil
}

// admit turns a message into a running task: it finds or opens the session, creates the task
// with the events kagent opens a stream with, and starts the turn.
//
// A message whose messageId was accepted before is a replay: if it names no contextId it joins
// that task and runs no turn (WP-02, measured; see gate for one that does). A message that is
// rejected is not remembered, so it can be resent. UNCONFIRMED: that a unary SendMessage
// replay blocks until the task's final snapshot, as a stream would, and that replaying the
// messageId of a cancelled task answers its CANCELED snapshot.
//
// A Reply function runs outside the lock, so it may call the Server. The lock is open while it
// runs, so the checks are made again after it: it may have run for a message that is then
// rejected, or that turns out to be a replay.
func (a *Agent) admit(m obj, owner string) (*stream, *rpcError) {
	if err := validateMessage(m); err != nil {
		return nil, err
	}
	id, _ := m["messageId"].(string)
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st, sess, err := a.gate(id, m, owner)
	if st != nil || err != nil {
		return st, err
	}
	steps, reply, ok := a.pick()
	if !ok {
		return nil, &rpcError{codeUnscripted, "stubagent: no turn scripted for " + a.ns + "/" + a.name, "STUBAGENT_UNSCRIPTED"}
	}
	if reply != nil {
		s.mu.Unlock()
		steps = reply(promptText(m))
		s.mu.Lock()
		if st, sess, err = a.gate(id, m, owner); st != nil || err != nil {
			return st, err
		}
	}
	return a.start(id, m, sess, steps), nil
}

// gate decides what a message that is not yet a task may do: join the task of a replayed
// messageId, resume a task it replies to, or be rejected (denied, busy). Otherwise it returns
// the session the message starts a task in. The caller holds s.mu.
func (a *Agent) gate(id string, m obj, owner string) (*stream, *session, *rpcError) {
	taskID, _ := m["taskId"].(string)
	ctxID, _ := m["contextId"].(string)
	// A replay joins the task it started, but only one that names no contextId is deduplicated:
	// WP-02 measured that (session, inflight). A replay that carries a contextId is a new
	// message to kagent: busy while the task runs, a second run of the turn after it ended
	// (measured by WP-03, oidc-replay; spec 8.2).
	// UNCONFIRMED: a replay of a reply to a task, which carries a taskId, is deduplicated
	// whatever else it carries; that is not measured.
	if prior := a.messages[id]; prior != nil && (ctxID == "" || taskID != "") {
		if prior.sess.owner != owner {
			return nil, nil, deniedError()
		}
		return prior.join(), nil, nil
	}
	if taskID != "" {
		st, err := a.resume(id, taskID, m, owner)
		return st, nil, err
	}
	sess, err := a.session(ctxID, owner)
	if err != nil {
		return nil, nil, err
	}
	if a.busy(sess) {
		return nil, nil, busyError("resource conflict")
	}
	return nil, sess, nil
}

// start creates the task for a message and starts its turn. The caller holds s.mu.
func (a *Agent) start(id string, m obj, sess *session, steps []Step) *stream {
	s := a.s
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
	return &stream{t: t}
}

// resume delivers a reply to the task it names. The caller holds s.mu.
//
// Only a task that is waiting for input takes a reply. One that is working, including one
// wedged by an earlier bad reply, is busy (WP-02, measured).
// UNCONFIRMED: a reply to a task that has ended is answered with -32004 too.
func (a *Agent) resume(id, taskID string, m obj, owner string) (*stream, *rpcError) {
	t := a.tasks[taskID]
	switch {
	case t == nil:
		return nil, taskNotFound()
	case t.sess.owner != owner:
		return nil, deniedError()
	case t.state == working:
		return nil, busyError("resource conflict")
	case t.pause == nil:
		return nil, busyError("task is not waiting for input")
	}
	p := t.pause
	payload, _ := dig(m, "metadata", HITLExtension).(obj)
	resp, bad := p.decode(payload)
	resp.Text = promptText(m)
	t.history = append(t.history, m)
	t.pause = nil
	t.setState(working, a.s.stamp())
	a.messages[id] = t
	if bad != nil {
		// kagent fails once the task is back in WORKING, and nothing runs to move it on. The
		// error goes to this reply's stream, after the WORKING update; only CancelTask ends it.
		return &stream{t: t, first: t.workingEvent(nil), end: true, fail: bad}, nil
	}
	from := len(t.log)
	t.push(t.workingEvent(nil), false)
	if !resp.Plain() {
		t.push(t.workingEvent(m), false) // kagent echoes a reply it took as an answer, not plain text
	}
	t.push(t.statusEvent(), false)
	a.launch(t, p.then(resp))
	return &stream{t: t, from: from}, nil
}

// busy reports whether a task of the session is working. kagent allows one at a time.
// UNCONFIRMED: a task that is INPUT_REQUIRED does not make its session busy.
func (a *Agent) busy(sess *session) bool {
	for _, t := range a.tasks {
		if t.sess == sess && t.state == working {
			return true
		}
	}
	return false
}

// find returns the task that params name, if owner may use it. The caller holds s.mu.
func (a *Agent) find(params json.RawMessage, owner string) (*task, *rpcError) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return nil, invalidParams("params.id is required")
	}
	t := a.tasks[p.ID]
	switch {
	case t == nil:
		return nil, taskNotFound()
	case t.sess.owner != owner:
		return nil, deniedError()
	}
	return t, nil
}

// getTask answers with the task as it is now, its text so far included.
func (a *Agent) getTask(o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params, o.owner)
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

// cancelTask stops the task's turn and ends its streams with the CANCELED task. The result is
// the CANCELED task with the text so far (busy/04, measured).
// UNCONFIRMED: an already open stream ending with the CANCELED task snapshot, and cancelling a
// task that is already completed or cancelled answering -32002.
func (a *Agent) cancelTask(o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params, o.owner)
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

// session finds the session for a contextId, or opens one for owner. An empty contextId gets a
// new id. A session is its owner's alone.
// UNCONFIRMED: a contextId that names no session opens one with that id.
func (a *Agent) session(ctxID, owner string) (*session, *rpcError) {
	if ctxID == "" {
		ctxID = a.s.newID("context")
	}
	if a.sessions[ctxID] == nil {
		a.sessions[ctxID] = &session{id: ctxID, owner: owner}
	}
	if a.sessions[ctxID].owner != owner {
		return nil, deniedError()
	}
	return a.sessions[ctxID], nil
}

// launch plays steps for the task in the background. The caller holds s.mu.
func (a *Agent) launch(t *task, steps []Step) {
	ctx, stop := context.WithCancel(a.s.ctx)
	t.stop, t.done = stop, make(chan struct{})
	done := t.done
	a.s.wg.Add(1)
	go func() {
		defer a.s.wg.Done()
		defer close(done)
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
// what happens next. For a task that is not running, the snapshot is the whole answer: measured
// for a completed task (inflight/03).
// UNCONFIRMED: the same for a task that was cancelled or is paused.
func (a *Agent) subscribeTask(ctx context.Context, o *rw, params json.RawMessage) {
	a.s.mu.Lock()
	t, err := a.find(params, o.owner)
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
	first obj       // the event that opens a join, or nil
	from  int       // the first log index to send after it
	end   bool      // first is the whole answer
	fail  *rpcError // ends the stream, after first, with this error
}

// follow writes the stream until a final event, the client going away, or the server closing.
// It reports whether the stream ended by itself: false means the client left or the server
// closed first, and what was written is not the outcome of the turn. The turn runs on
// regardless of the client.
func (s *Server) follow(ctx context.Context, o *rw, st *stream) bool {
	if st.first != nil {
		o.event(st.first)
		if st.fail != nil {
			o.fail(st.fail)
		}
		if st.end {
			return true
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
				return true
			}
		}
		from += len(evs)
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		case <-s.ctx.Done():
			return false
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
