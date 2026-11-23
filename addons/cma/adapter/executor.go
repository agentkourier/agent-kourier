package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// HITLExtension is kagent's human-in-the-loop A2A extension. Agent Kourier's a2a dialect reads a pause marked with it
// as a structured question or tool approval, and answers with it.
const HITLExtension = "https://kagent.dev/extensions/hitl/v1"

// DefaultKeepalive is how long the adapter lets a turn go without an event before it sends a working update. Agent
// Kourier cuts a stream that is silent for 5 minutes, and a self-hosted sandbox can be silent longer than that while
// it moves a session to a new pod.
const DefaultKeepalive = 60 * time.Second

// Executor runs A2A tasks as turns of Managed Agents sessions, as the kit's runtime runs a backend (package runtime):
// the runtime counts and traces each Execute as a turn, replays a resent message, and keeps the tasks. One A2A context
// is one session, created on the context's first message with the context ID in its metadata. The context-to-session
// map is kept in Store, with a copy in memory. The runtime calls the executor's hooks: Restore when this replica takes
// over as the leader, to reconcile the map with CMA; Interrupted once it has failed the turns a restart cut off;
// ValidateResume before an answer to a paused task runs; ReleaseContext for each context idle past SESSION_IDLE_TTL;
// FailedOutcome to name a timed-out turn.
type Executor struct {
	Backend Backend
	// Store keeps each context's session across a restart. Required.
	Store Store
	// Keepalive is the silence after which a working update is sent. Zero is DefaultKeepalive.
	Keepalive time.Duration
	// IdleTTL is SESSION_IDLE_TTL, which the log line of an idle session's archive names. The runtime's upkeep decides
	// when a context is idle.
	IdleTTL time.Duration
	// ToolResultTimeout is how long a turn waits, with no session event, for the result of a tool call the session
	// runs in its sandbox (one the agent may run, or one a person approved). Past it the adapter interrupts the
	// session and fails the turn: a sandbox that cannot start sends nothing at all. A tool that runs silently for
	// longer is cut off the same way. Zero waits for ever.
	ToolResultTimeout time.Duration
	// Log receives what the adapter cannot tell the client. Nil is slog.Default().
	Log *slog.Logger
	// Metrics counts the sessions let go of. Nil records nothing. Wrap Backend in Observe to time and trace the calls
	// to the API.
	Metrics *Metrics

	now      func() time.Time // nil is time.Now
	mu       sync.Mutex
	sessions map[string]*contextSession  // by A2A context ID; Store's map, plus the turns running here
	creating map[string]chan struct{}    // by A2A context ID: a session being created, closed once that is over
	canceled map[a2a.TaskID]*atomic.Bool // the turns running here, by task: set by Cancel before it interrupts
}

// contextSession is a context's session and its activity.
type contextSession struct {
	id         string
	lastActive time.Time // when a turn of it last started or ended, as TouchSession stored it
	turns      int       // turns running now; a session with one is never archived
}

var (
	_ a2asrv.AgentExecutor    = (*Executor)(nil)
	_ runtime.Restorer        = (*Executor)(nil)
	_ runtime.Interrupter     = (*Executor)(nil)
	_ runtime.ResumeValidator = (*Executor)(nil)
	_ runtime.ContextReleaser = (*Executor)(nil)
	_ runtime.TurnClassifier  = (*Executor)(nil)
)

// Execute sends the message, or the answer to a paused task, to the context's session and translates the session's
// events into A2A events until the turn ends or pauses.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		out, known, err := e.outbound(ec)
		if err != nil {
			yield(nil, err)
			return
		}
		canceled := e.startTurn(ec.TaskID)
		defer e.endTurn(ec.TaskID, canceled)
		session, err := e.acquire(ctx, ec.ContextID)
		if err != nil {
			yield(nil, fmt.Errorf("start session: %w", err))
			return
		}
		defer e.release(ctx, ec.ContextID, session)
		// Open the stream before sending: it delivers only what happens after it opens.
		stream, err := e.Backend.Stream(ctx, session)
		if err != nil {
			yield(nil, fmt.Errorf("open session stream: %w", err))
			return
		}
		defer func() { _ = stream.Close() }()
		if err := e.Backend.Send(ctx, session, out...); err != nil {
			yield(nil, fmt.Errorf("send to session: %w", err))
			return
		}
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		t := &turnState{ec: ec, session: session, names: known, asks: map[string]Event{}, running: map[string]bool{},
			canceled: canceled, log: e.logger().With("session", session, "task", ec.TaskID)}
		for _, o := range out {
			if c := o.Confirmation; c != nil && c.Allow {
				t.running[c.ToolUseID] = true
			}
		}
		e.follow(ctx, stream, t, yield)
	}
}

// startTurn registers a running turn of task, which Cancel can mark canceled.
func (e *Executor) startTurn(task a2a.TaskID) *atomic.Bool {
	canceled := &atomic.Bool{}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.canceled == nil {
		e.canceled = map[a2a.TaskID]*atomic.Bool{}
	}
	e.canceled[task] = canceled
	return canceled
}

// endTurn forgets a turn startTurn registered.
func (e *Executor) endTurn(task a2a.TaskID, canceled *atomic.Bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.canceled[task] == canceled {
		delete(e.canceled, task)
	}
}

// Contexts is how many contexts have a session in the executor's map, for the sessions gauge.
func (e *Executor) Contexts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

// Cancel interrupts the context's session. The session stops at its next safe point.
func (e *Executor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.mu.Lock()
		cs, ok := e.sessions[ec.ContextID]
		if canceled := e.canceled[ec.TaskID]; canceled != nil {
			canceled.Store(true)
		}
		e.mu.Unlock()
		if ok {
			if err := e.Backend.Send(ctx, cs.id, Outbound{Interrupt: true}); err != nil {
				yield(nil, fmt.Errorf("interrupt session: %w", err))
				return
			}
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *Executor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Executor) keepalive() time.Duration {
	if e.Keepalive > 0 {
		return e.Keepalive
	}
	return DefaultKeepalive
}

// acquire returns the context's session, creating and storing it on first use, and counts a turn on it until
// release. A session it fails to store is still used: Restore finds it again by its metadata after a restart.
//
// The creation runs outside e.mu, so a slow CreateSession holds back no other context. Another turn of the same
// context waits for it, and finds the session in the map and the store; if the creation failed, the turn tries one
// of its own.
func (e *Executor) acquire(ctx context.Context, contextID string) (string, error) {
	for {
		e.mu.Lock()
		if cs, ok := e.sessions[contextID]; ok {
			cs.turns++
			cs.lastActive = e.clock()
			id, at := cs.id, cs.lastActive
			e.mu.Unlock()
			e.touch(ctx, contextID, id, at)
			return id, nil
		}
		if pending, ok := e.creating[contextID]; ok {
			e.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		done := make(chan struct{})
		if e.creating == nil {
			e.creating = map[string]chan struct{}{}
		}
		e.creating[contextID] = done
		e.mu.Unlock()
		return e.create(ctx, contextID, done)
	}
}

// create makes the context's session, stores it, and puts it in the map with one turn counted. done, the context's
// entry in e.creating, is closed once the map has the session or the creation has failed.
func (e *Executor) create(ctx context.Context, contextID string, done chan struct{}) (string, error) {
	defer close(done)
	finish := func(cs *contextSession) {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.creating, contextID)
		if cs != nil {
			if e.sessions == nil {
				e.sessions = map[string]*contextSession{}
			}
			e.sessions[contextID] = cs
		}
	}
	id, err := e.Backend.CreateSession(ctx, "A2A context "+contextID, map[string]string{contextMetadataKey: contextID})
	if err != nil {
		finish(nil)
		return "", err
	}
	cs := &contextSession{id: id, lastActive: e.clock(), turns: 1}
	// Stored before the map has it, so that no other turn of the context can run before the store knows its session.
	if err := e.Store.PutSession(ctx, store.Session{ContextID: contextID, SessionID: id, LastActive: cs.lastActive}); err != nil {
		e.logger().Warn("store a new session", "context", contextID, "session", id, "error", err)
	}
	finish(cs)
	return id, nil
}

// release ends a turn that acquire counted.
func (e *Executor) release(ctx context.Context, contextID, session string) {
	e.mu.Lock()
	cs, ok := e.sessions[contextID]
	if !ok || cs.id != session {
		e.mu.Unlock()
		return
	}
	cs.turns--
	cs.lastActive = e.clock()
	at := cs.lastActive
	e.mu.Unlock()
	e.touch(context.WithoutCancel(ctx), contextID, session, at)
}

// touch stores a session's last activity, which the runtime's upkeep counts the idle time from.
func (e *Executor) touch(ctx context.Context, contextID, session string, at time.Time) {
	if err := e.Store.TouchSession(ctx, contextID, session, at); err != nil {
		e.logger().Warn("store a session's activity", "context", contextID, "session", session, "error", err)
	}
}

func (e *Executor) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// outbound returns what to send for the request: the decisions of a tool approval when the task is paused on one,
// and otherwise the message's text. known names the tools of the pause, so their results render with a name.
func (e *Executor) outbound(ec *a2asrv.ExecutorContext) (out []Outbound, known map[string]string, err error) {
	known = map[string]string{}
	if ec.Message == nil {
		return nil, nil, a2a.ErrInvalidParams
	}
	if task := ec.StoredTask; task != nil && task.Status.State == a2a.TaskStateInputRequired {
		pending, err := pendingTools(task)
		if err != nil {
			return nil, nil, err
		}
		decisions, err := approvals(ec.Message)
		if err != nil {
			return nil, nil, err
		}
		if len(decisions) != len(pending) {
			return nil, nil, fmt.Errorf("%w: %d of %d tool calls decided", a2a.ErrInvalidParams, len(decisions), len(pending))
		}
		for _, p := range pending {
			d, ok := decisions[p.ID]
			if !ok {
				return nil, nil, fmt.Errorf("%w: a pending tool call is not decided", a2a.ErrInvalidParams)
			}
			c := &Confirmation{ToolUseID: p.ID, Allow: d.Approved}
			if !d.Approved {
				c.DenyMessage = d.Reason
			}
			out = append(out, Outbound{Confirmation: c})
			known[p.ID] = p.Name
		}
		return out, known, nil
	}
	var text []string
	for _, p := range ec.Message.Parts {
		switch c := p.Content.(type) {
		case a2a.Text:
			text = append(text, string(c))
		case a2a.Data:
			raw, err := json.Marshal(c.Value)
			if err != nil {
				return nil, nil, a2a.ErrInvalidParams
			}
			text = append(text, string(raw))
		default:
			return nil, nil, fmt.Errorf("%w: only text and data parts are supported", a2a.ErrInvalidParams)
		}
	}
	msg := strings.TrimSpace(strings.Join(text, "\n"))
	if msg == "" {
		return nil, nil, fmt.Errorf("%w: empty message", a2a.ErrInvalidParams)
	}
	return []Outbound{UserMessage(msg)}, known, nil
}

// turnState is what one turn has seen of the session.
type turnState struct {
	ec      *a2asrv.ExecutorContext
	session string
	started bool              // the session has run since the send; an idle before that is stale
	names   map[string]string // tool-use event ID -> tool name
	asks    map[string]Event  // tool uses waiting on a person, by event ID
	running map[string]bool   // tool uses the session runs, waiting on their result, by event ID
	// canceled is set by Cancel before it interrupts the session; nil reads as not canceled.
	canceled *atomic.Bool
	log      *slog.Logger
}

// follow translates the stream's events until the turn settles, sending a working update after each silence of
// keepalive.
func (e *Executor) follow(ctx context.Context, stream EventStream, t *turnState, yield func(a2a.Event, error) bool) {
	events := make(chan Event)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(events)
		for stream.Next() {
			select {
			case events <- stream.Current():
			case <-stop:
				return
			}
		}
	}()
	tick := time.NewTicker(e.keepalive())
	defer tick.Stop()
	// stall fires ToolResultTimeout after the last event; it ends the turn only while a tool result is awaited.
	var stall <-chan time.Time
	var stallTimer *time.Timer
	if e.ToolResultTimeout > 0 {
		stallTimer = time.NewTimer(e.ToolResultTimeout)
		defer stallTimer.Stop()
		stall = stallTimer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !yield(a2a.NewStatusUpdateEvent(t.ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
		case <-stall:
			if len(t.running) == 0 {
				stallTimer.Reset(e.ToolResultTimeout)
				continue
			}
			t.log.Warn("no tool result; interrupting the session", "timeout", e.ToolResultTimeout, "tool_uses", len(t.running))
			if err := e.Backend.Send(context.WithoutCancel(ctx), t.session, Outbound{Interrupt: true}); err != nil {
				t.log.Warn("interrupt a stalled session", "error", err)
			}
			failed := t.status(a2a.TaskStateFailed, fmt.Sprintf("No result from the agent's sandbox for %s, so the adapter "+
				"interrupted the session. The sandbox may not have started: check the environment's capacity.", e.ToolResultTimeout))
			// Agent Kourier shows a backend's failure only as a fixed notice; this code selects its timeout notice.
			failed.Status.Message.Metadata = map[string]any{"code": "timeout"}
			yield(failed, nil)
			return
		case ev, ok := <-events:
			if !ok {
				err := stream.Err()
				if err == nil {
					err = errors.New("session stream ended before the turn did")
				}
				t.log.Warn("session stream ended", "error", err)
				yield(t.status(a2a.TaskStateFailed, "The connection to the agent's session was lost."), nil)
				return
			}
			tick.Reset(e.keepalive())
			if stallTimer != nil {
				stallTimer.Reset(e.ToolResultTimeout)
			}
			out, done := t.translate(ev)
			for _, o := range out {
				if !yield(o, nil) {
					return
				}
			}
			if done {
				return
			}
		}
	}
}

// translate returns the A2A events for one session event, and whether the turn has ended or paused.
func (t *turnState) translate(ev Event) ([]a2a.Event, bool) {
	switch ev.Type {
	case evStatusRunning:
		t.started = true
	case evAgentMessage:
		t.started = true
		if text := blocksText(ev.Content); text != "" {
			return []a2a.Event{a2a.NewArtifactEvent(t.ec, a2a.NewTextPart(text))}, false
		}
	case evAgentToolUse, evAgentMCPToolUse:
		t.started = true
		t.names[ev.ID] = ev.Name
		switch {
		case ev.EvaluatedPermission == permissionAsk:
			t.asks[ev.ID] = ev
		case ev.EvaluatedPermission != permissionDeny && ev.Type == evAgentToolUse:
			t.running[ev.ID] = true // a built-in tool, run in the sandbox; an MCP tool runs elsewhere
		}
		return []a2a.Event{a2a.NewArtifactEvent(t.ec, functionCall(ev))}, false
	case evAgentToolResult, evUserToolResult, evAgentMCPResult:
		id := ev.ToolUseID
		if ev.Type == evAgentMCPResult {
			id = ev.MCPToolUseID
		}
		delete(t.asks, id)
		delete(t.running, id)
		return []a2a.Event{a2a.NewArtifactEvent(t.ec, functionResponse(id, t.names[id], ev))}, false
	case evSessionError:
		if ev.Error != nil {
			t.log.Warn("session error", "type", ev.Error.Type, "message", ev.Error.Message)
		}
	case evStatusTerminated:
		return []a2a.Event{t.status(a2a.TaskStateFailed, "The agent's session ended.")}, true
	case evStatusIdle:
		if !t.started || ev.StopReason == nil {
			return nil, false
		}
		// After a cancel, the session idles because of its interrupt: the turn ends canceled, whichever of this update
		// and the cancel's own reaches the task first, so the cancel never loses to a completion it caused.
		if t.canceled != nil && t.canceled.Load() {
			return []a2a.Event{a2a.NewStatusUpdateEvent(t.ec, a2a.TaskStateCanceled, nil)}, true
		}
		switch ev.StopReason.Type {
		case stopEndTurn:
			return []a2a.Event{a2a.NewStatusUpdateEvent(t.ec, a2a.TaskStateCompleted, nil)}, true
		case stopRequiresAction:
			// On a self-hosted environment the session also idles on the calls its sandbox runs, until the worker
			// posts their results: those are waits, not questions for a person.
			var ask []string
			for _, id := range ev.StopReason.EventIDs {
				if !t.running[id] {
					ask = append(ask, id)
				}
			}
			if len(ask) == 0 && len(ev.StopReason.EventIDs) > 0 {
				return nil, false
			}
			pause, err := t.approvalRequest(ask)
			if err != nil {
				t.log.Warn("unanswerable pause", "reason", err)
				return []a2a.Event{t.status(a2a.TaskStateFailed, "The agent is waiting on an action this adapter cannot answer.")}, true
			}
			return []a2a.Event{pause}, true
		default:
			return []a2a.Event{t.status(a2a.TaskStateFailed, "The agent stopped: "+ev.StopReason.Type+".")}, true
		}
	}
	return nil, false
}

// status is a status update with an agent message.
func (t *turnState) status(state a2a.TaskState, text string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(t.ec, state, a2a.NewMessageForTask(a2a.MessageRoleAgent, t.ec, a2a.NewTextPart(text)))
}

// hitlTool is a tool call in a HITL tool_approval_request.
type hitlTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// approvalRequest is the input-required update for the tool calls the session waits on. Each must be a tool use this
// turn saw ask for a decision; anything else (a custom tool, a call from before the stream opened) cannot be answered.
func (t *turnState) approvalRequest(ids []string) (*a2a.TaskStatusUpdateEvent, error) {
	if len(ids) == 0 {
		return nil, errors.New("requires_action names no events")
	}
	tools := make([]hitlTool, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		ev, ok := t.asks[id]
		if !ok {
			return nil, fmt.Errorf("event %s is not a tool call waiting on approval", id)
		}
		tools = append(tools, hitlTool{ID: ev.ID, CallID: ev.ID, Name: ev.Name, Args: inputArgs(ev.Input)})
		names = append(names, ev.Name)
	}
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, t.ec, a2a.NewTextPart("Approve "+strings.Join(names, ", ")+"?"))
	msg.Extensions = []string{HITLExtension}
	payload, err := toMap(map[string]any{"type": "tool_approval_request", "tools": tools})
	if err != nil {
		return nil, err
	}
	msg.Metadata = map[string]any{HITLExtension: payload}
	return a2a.NewStatusUpdateEvent(t.ec, a2a.TaskStateInputRequired, msg), nil
}

// pendingTools reads the tool calls a paused task asked about from its status message.
func pendingTools(task *a2a.Task) ([]hitlTool, error) {
	msg := task.Status.Message
	if msg == nil {
		return nil, fmt.Errorf("%w: paused task has no request", a2a.ErrInvalidParams)
	}
	var req struct {
		Type  string     `json:"type"`
		Tools []hitlTool `json:"tools"`
	}
	if err := fromAny(msg.Metadata[HITLExtension], &req); err != nil || req.Type != "tool_approval_request" || len(req.Tools) == 0 {
		return nil, fmt.Errorf("%w: paused task has no tool approval request", a2a.ErrInvalidParams)
	}
	return req.Tools, nil
}

type decision struct {
	Approved bool
	Reason   string
}

// approvals reads a HITL tool_approval_response from the message. Plain text never approves a tool call.
func approvals(msg *a2a.Message) (map[string]decision, error) {
	var resp struct {
		Type      string `json:"type"`
		Approvals []struct {
			ID       string `json:"id"`
			Approved bool   `json:"approved"`
			Reason   string `json:"rejection_reason"`
		} `json:"approvals"`
	}
	if err := fromAny(msg.Metadata[HITLExtension], &resp); err != nil || resp.Type != "tool_approval_response" {
		return nil, fmt.Errorf("%w: a tool approval needs a tool_approval_response", a2a.ErrInvalidParams)
	}
	out := make(map[string]decision, len(resp.Approvals))
	for _, a := range resp.Approvals {
		if _, twice := out[a.ID]; twice || a.ID == "" {
			return nil, fmt.Errorf("%w: a tool call is decided twice or without an id", a2a.ErrInvalidParams)
		}
		out[a.ID] = decision{Approved: a.Approved, Reason: a.Reason}
	}
	return out, nil
}

// functionCall is a tool call as Google ADK's A2A converter writes it, which Agent Kourier renders as a tool call.
func functionCall(ev Event) *a2a.Part {
	p := a2a.NewDataPart(map[string]any{"id": ev.ID, "name": ev.Name, "args": inputArgs(ev.Input)})
	p.Metadata = map[string]any{"adk_type": "function_call"}
	return p
}

// functionResponse is a tool result as ADK writes it. Long output is cut to maxToolResultLength runes.
func functionResponse(id, name string, ev Event) *a2a.Part {
	out := blocksText(ev.Content)
	if r := []rune(out); len(r) > maxToolResultLength {
		out = string(r[:maxToolResultLength]) + "…"
	}
	resp := map[string]any{"output": out}
	if ev.IsError {
		resp["is_error"] = true
	}
	p := a2a.NewDataPart(map[string]any{"id": id, "name": name, "response": resp})
	p.Metadata = map[string]any{"adk_type": "function_response"}
	return p
}

func blocksText(blocks []ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// inputArgs is a tool's input as an object; input that is not one is kept under "input".
func inputArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil && m != nil {
		return m
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return map[string]any{"input": v}
}

func toMap(v any) (map[string]any, error) {
	var m map[string]any
	return m, fromAny(v, &m)
}

func fromAny(v any, out any) error {
	if v == nil {
		return errors.New("missing")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
