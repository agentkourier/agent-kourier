package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// assembler folds the events of one send, and of every reconnect within it, into Events.
// A task snapshot (from GetTask, SubscribeToTask, or a replayed send) and a live chunk
// take the same path, so the same text never arrives twice: an artifact chunk with
// append: true adds to the artifact's text, and anything else replaces it (spec 8.3).
// Tool parts follow the rules at (*assembler).tool.
type assembler struct {
	emit      Handler
	text      map[a2a.ArtifactID]string
	placed    map[toolPlace]bool               // the places of the status messages whose tool part was looked at
	sent      map[artifactPart]int             // how many tool parts of each saying each artifact has delivered
	said      map[[sha256.Size]byte]toolSource // what each tool part delivered or held said, and where it was found
	called    map[string]bool                  // the call IDs whose call was delivered
	live      map[toolKey]bool                 // the calls, as a source reports them, that had a result from a live event
	held      []ToolResultEvent                // results whose call has not come, in the order they came
	snapshot  map[toolKey]toolPlace            // while a task snapshot is applied: the place of the last result of each call; else nil
	paused    *a2a.Task                        // the task a tool approval answers, as the agent held it before the answer; nil if not an approval, or if unread
	taskID    a2a.TaskID
	contextID string
	state     a2a.TaskState
	final     *a2a.Task
	changes   int  // events emitted so far: text, tool, progress, state, and final
	replies   bool // a task-less agent Message may be the reply: set for a Send, never for an Answer or a follow
	unshown   bool // an agent Message came before any task where it cannot be the reply: on an Answer
	// interaction reads the pause of the task that ends the send (Config.ParseInteraction); nil reads none.
	interaction func(a2a.Task) (*dialect.Interaction, error)
}

func newAssembler(h Handler) *assembler {
	if h == nil {
		h = func(Event) {}
	}
	a := &assembler{text: map[a2a.ArtifactID]string{}, placed: map[toolPlace]bool{}, sent: map[artifactPart]int{}, said: map[[sha256.Size]byte]toolSource{},
		called: map[string]bool{}, live: map[toolKey]bool{}}
	a.emit = func(e Event) {
		a.changes++
		h(e)
	}
	return a
}

// apply folds one event from the agent into the assembler and emits what changed.
func (a *assembler) apply(ev a2a.Event) {
	switch e := ev.(type) {
	case *a2a.Task:
		a.track(e.ID, e.ContextID)
		a.snapshot = lastResults(e)
		for _, m := range snapshotMessages(e) {
			a.messageTools(m)
		}
		for _, art := range e.Artifacts {
			a.artifact(art, false, true)
		}
		a.snapshot = nil
		a.setState(e.Status.State)
		if settled(e.Status.State) && a.final == nil {
			a.end(e)
		}
	case *a2a.TaskStatusUpdateEvent:
		a.track(e.TaskID, e.ContextID)
		if readable(e.Status.State, e.Status.Message) {
			a.messageTools(e.Status.Message)
		}
		changed := e.Status.State != a.state
		a.setState(e.Status.State)
		if e.Status.State == a2a.TaskStateWorking && !toldByTools(e.Status.Message) {
			a.progress(e.Status.Message, changed)
		}
	case *a2a.TaskArtifactUpdateEvent:
		a.track(e.TaskID, e.ContextID)
		a.artifact(e.Artifact, e.Append, false)
	case *a2a.Message:
		a.message(e)
	}
}

// message folds a reply that is one Message and no task (spec 6.3, 8.3): the agent ran the turn
// and has nothing more to send. It is told as a task that completed with the message as its
// status, under no task ID, in the message's context: the state first (the turn is the agent's
// now), then the message's parts as one artifact named after the message, then the end.
// Any other Message is ignored, so it changes nothing: one on an Answer or a follow, whose task
// is known and must not be forgotten, one that names a task, one not from the agent (an echo of
// the turn), and one after the agent has named a task. An agent's Message on an Answer before
// any task is noted (unshown), so the send can say that the agent did reply.
func (a *assembler) message(m *a2a.Message) {
	if m.Role != a2a.MessageRoleAgent || a.taskID != "" {
		return
	}
	if !a.replies {
		a.unshown = true
		return
	}
	if m.TaskID != "" {
		return
	}
	a.track("", m.ContextID)
	a.setState(a2a.TaskStateCompleted)
	a.artifact(&a2a.Artifact{ID: a2a.ArtifactID(m.ID), Parts: m.Parts}, false, false)
	a.end(&a2a.Task{ContextID: a.contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: m}})
}

// end delivers the results still waiting for a call that never came, then the end of the send.
func (a *assembler) end(task *a2a.Task) {
	a.release("")
	a.final = task
	a.emit(FinalEvent{Task: task, AwaitingApproval: a.awaitingApproval(task)})
}

// awaitingApproval is the call IDs of the tool calls that a task paused for a tool approval waits on, as the dialect
// reads the pause. A pause it cannot read, a question, and an approval a sub-agent raised name none: a sub-agent's calls
// are not this stream's, and one may have the ID of a call of this stream.
func (a *assembler) awaitingApproval(task *a2a.Task) []string {
	if a.interaction == nil || task == nil || task.Status.State != a2a.TaskStateInputRequired {
		return nil
	}
	in, err := a.interaction(*task)
	if err != nil || in == nil || in.Kind != dialect.ToolApproval || in.Nested != nil {
		return nil
	}
	var ids []string
	for _, t := range in.Tools {
		if t.CallID != "" {
			ids = append(ids, t.CallID)
		}
	}
	return ids
}

// settled reports whether a task has nothing more to stream: it ended or awaits input.
func settled(s a2a.TaskState) bool {
	return s.Terminal() || s == a2a.TaskStateInputRequired || s == a2a.TaskStateAuthRequired
}

func (a *assembler) track(id a2a.TaskID, contextID string) {
	if id != "" {
		a.taskID = id
	}
	if contextID != "" {
		a.contextID = contextID
	}
}

func (a *assembler) setState(s a2a.TaskState) {
	if s == "" || s == a.state {
		return
	}
	a.state = s
	a.emit(StateEvent{TaskID: a.taskID, ContextID: a.contextID, State: s})
}

// artifact applies one artifact, or one chunk of it: its text parts per the append rule,
// and its tool parts once each. A task snapshot that is a strict prefix of the text held is
// older than it (kagent never shortens an artifact's text within a turn), so it is skipped.
func (a *assembler) artifact(art *a2a.Artifact, appendMode, snapshot bool) {
	if art == nil {
		return
	}
	var text strings.Builder
	hasText := false
	seen := map[[sha256.Size]byte]int{} // how many parts of each saying this chunk has held so far
	for i, p := range art.Parts {
		if tp, ok := decodeTool(p); ok {
			a.artifactTool(art.ID, i, tp, appendMode, seen)
		} else if t, isText := p.Content.(a2a.Text); isText {
			text.WriteString(string(t))
			hasText = true
		}
	}
	if !hasText {
		return
	}
	old, t := a.text[art.ID], text.String()
	if appendMode {
		t = old + t
	}
	stale := snapshot && strings.HasPrefix(old, t)
	if t != old && !stale {
		a.text[art.ID] = t
		a.emit(TextEvent{ArtifactID: art.ID, Text: t})
	}
}

// artifactTool delivers a tool part of an artifact chunk if the artifact has not delivered it. A chunk that adds to the
// artifact (append: true) holds parts that follow the ones sent, so each is new, though its index restarts at 0: Google
// ADK's default output is one artifact for a whole run, of which every call and result is such a chunk. A chunk that
// replaces the artifact, and a task snapshot, hold all its parts again, so a part is new only past the number of its
// saying that the artifact has delivered already: that tells a part from its repeat wherever the replace puts it, and
// keeps two calls that say the same (no call ID, same arguments) two. seen counts the parts of this chunk so far.
func (a *assembler) artifactTool(art a2a.ArtifactID, index int, tp toolPart, appendMode bool, seen map[[sha256.Size]byte]int) {
	key := artifactPart{art, tp.says}
	if appendMode {
		a.sent[key]++
	} else {
		seen[tp.says]++
		if seen[tp.says] <= a.sent[key] {
			return
		}
		a.sent[key] = seen[tp.says]
	}
	a.tool(artifactPlace(art, index), art, tp)
}

// messageTools delivers the tool parts of a message of the agent's, once each.
func (a *assembler) messageTools(m *a2a.Message) {
	for i, p := range m.Parts {
		tp, ok := decodeTool(p)
		if !ok {
			continue
		}
		place := messagePlace(m, i, tp)
		if !a.placed[place] {
			a.placed[place] = true
			a.tool(place, "", tp)
		}
	}
}

// progress delivers a live working update that holds no tool part as the agent's activity (ProgressEvent), with the text
// of its message if the message is the agent's. A text part marked as the model's reasoning (dialect.IsThought) is not
// read: ADK's older converter, which kagent 0.9.x uses, sends every part of the run in working updates, its reasoning
// among them. changed says the update made the task working: that is the change of state, which StateEvent has told, and
// it is progress only if it says something.
func (a *assembler) progress(m *a2a.Message, changed bool) {
	var text []string
	if readable(a2a.TaskStateWorking, m) {
		for _, p := range m.Parts {
			if t, ok := p.Content.(a2a.Text); ok && !dialect.IsThought(p) {
				text = append(text, string(t))
			}
		}
	}
	if changed && len(text) == 0 {
		return
	}
	a.emit(ProgressEvent{Text: strings.Join(text, "\n")})
}

// toldByTools says whether the message of a working update is the agent's and holds a tool part: the update is told by its
// tool events, and is no ProgressEvent. Any other message's tool parts are not read, so its update is progress like any other.
func toldByTools(m *a2a.Message) bool {
	if !readable(a2a.TaskStateWorking, m) {
		return false
	}
	for _, p := range m.Parts {
		if _, ok := dialect.DecodeToolPart(p); ok {
			return true
		}
	}
	return false
}

// readable says whether a status message is read for tool parts: the agent's own message while the task works, which
// is where kagent 0.9.x and Google ADK's older A2A converter send tool calls and results
// (test/fixtures/kagent/v09-tools). The user's message echoed back in a status update is not the agent's, and the
// message of a pause or of the end of the turn is the question or the answer, not progress. The message's text is
// not read: 0.9.x repeats there the answer that its artifact holds.
func readable(state a2a.TaskState, m *a2a.Message) bool {
	return m != nil && m.Role == a2a.MessageRoleAgent && state == a2a.TaskStateWorking
}

// snapshotMessages are the messages of a task snapshot that are read for tool parts, oldest first: the agent's messages in
// its history since the last message of the user, which say no state of their own and so are read whatever the task's,
// and its status message if it is readable. A task that went on from an earlier turn holds that turn's messages in
// its history, before the user's message that began this one, and they are not this turn's news. A message in both
// is read once.
func snapshotMessages(t *a2a.Task) []*a2a.Message {
	var out []*a2a.Message
	since := 0
	for i, m := range t.History {
		if m != nil && m.Role == a2a.MessageRoleUser {
			since = i + 1
		}
	}
	for _, m := range t.History[since:] {
		if m != nil && m.Role == a2a.MessageRoleAgent {
			out = append(out, m)
		}
	}
	if readable(t.Status.State, t.Status.Message) {
		out = append(out, t.Status.Message)
	}
	return out
}

// toolSource is where a tool part was found: the two places an agent puts one.
type toolSource uint8

const (
	inArtifact toolSource = iota
	inMessage             // a status message, or a message in a task's history
)

// toolPlace is where a tool part sits in a message or in a snapshot's artifact, which is its identity there.
type toolPlace struct {
	source toolSource
	owner  string // the artifact's ID, or the message's
	index  int    // the part's index in the artifact or message
	// says is what the part says, set for a message with no ID, which has no place but its words: two such
	// messages are one only if their parts say the same.
	says [sha256.Size]byte
}

// toolKey names the call that a result answers, as one source reports it.
type toolKey struct {
	source toolSource
	id     string
}

// artifactPart names the parts of one artifact that say the same.
type artifactPart struct {
	art  a2a.ArtifactID
	says [sha256.Size]byte
}

func artifactPlace(id a2a.ArtifactID, index int) toolPlace {
	return toolPlace{source: inArtifact, owner: string(id), index: index}
}

func messagePlace(m *a2a.Message, index int, tp toolPart) toolPlace {
	if m.ID == "" {
		return toolPlace{source: inMessage, index: index, says: tp.says}
	}
	return toolPlace{source: inMessage, owner: m.ID, index: index}
}

// toolPart is a decoded tool part with a digest of what it says: its kind, call ID and name, and its arguments or
// response. A digest, so that the assembler keeps 32 bytes of each part for the whole send and not its arguments and
// response.
type toolPart struct {
	dialect.ToolPart
	says [sha256.Size]byte
}

func decodeTool(p *a2a.Part) (toolPart, bool) {
	tp, ok := dialect.DecodeToolPart(p)
	if !ok {
		return toolPart{}, false
	}
	b, err := json.Marshal([]any{tp.Kind, tp.ID, tp.Name, tp.Args, tp.Result})
	if err != nil { // unreachable for a part decoded from JSON
		b = fmt.Appendf(nil, "%#v", tp)
	}
	return toolPart{tp, sha256.Sum256(b)}, true
}

// eachToolPart calls visit for each tool part a task snapshot holds, in the order they are read: its messages, then its
// artifacts. A part of an artifact is at its index in the artifact, which a snapshot holds whole.
func eachToolPart(t *a2a.Task, visit func(place toolPlace, art a2a.ArtifactID, tp toolPart)) {
	for _, m := range snapshotMessages(t) {
		for i, p := range m.Parts {
			if tp, ok := decodeTool(p); ok {
				visit(messagePlace(m, i, tp), "", tp)
			}
		}
	}
	for _, art := range t.Artifacts {
		if art == nil {
			continue
		}
		for i, p := range art.Parts {
			if tp, ok := decodeTool(p); ok {
				visit(artifactPlace(art.ID, i), art.ID, tp)
			}
		}
	}
}

// lastResults is the place of the last result of each call that a task snapshot holds, as a source orders them: its
// messages as eachToolPart reads them, and its artifacts in the order they came.
func lastResults(t *a2a.Task) map[toolKey]toolPlace {
	last := map[toolKey]toolPlace{}
	eachToolPart(t, func(place toolPlace, _ a2a.ArtifactID, tp toolPart) {
		if tp.Kind == dialect.ToolResult && tp.ID != "" {
			last[toolKey{place.source, tp.ID}] = place
		}
	})
	return last
}

// tool delivers one tool part that is not a repeat within its source, art being its artifact if it came in one. Each
// part is delivered once, however often a snapshot or a status update repeats it, and its meaning is kept:
//
//   - Within one source a part is told from its repeat by where it is: its message and index, and, in an artifact, its
//     saying and how many parts of that saying the artifact has delivered (artifactTool). kagent 1.0 sends two results
//     for one call ID, as two artifacts, when a call is approved (test/fixtures/kagent/approval), and both are the
//     agent's account.
//   - Across the sources, what the part says is its identity: a part an agent sends both as an artifact and as a
//     status message is delivered once. A part of the same kind and call ID that says something else, an
//     outcome the first did not have, is another part and is delivered; a part with no call ID is what it says.
//   - A task snapshot is history, and the live events are the news. kagent 1.0 replays the paused turn's call and its
//     "requires confirmation" error in the final task of the turn that approves it, and that error must not end the
//     approved call as failed. Two rules keep it out, each for a case the other does not cover. Of several results
//     of one call in a snapshot, the last is the outcome: the recorded final task holds the error and then the real
//     result, and a snapshot read after a restart has no live stream to compare with. A snapshot's result for a call
//     whose result a live event of the same source already delivered is stale: a snapshot that lags the stream can
//     hold the error and not yet the real result, which streamed, so last-wins alone would deliver the error.
//   - A result is delivered after its call, which a step needs. A live result whose call this send has not delivered is
//     first looked for in the task the send answers, when the send is a tool approval (fetchCall): the call of an approved
//     result was sent by the turn that paused, and is delivered then, so the step begins before the text that follows. A
//     result whose call is nowhere waits for it, and is delivered when the send ends, or fails, if the call never comes.
//   - A task that went on from a turn that paused holds that turn's tool parts, which are not this turn's news (seed).
func (a *assembler) tool(place toolPlace, art a2a.ArtifactID, tp toolPart) {
	if first, seen := a.said[tp.says]; seen && first != place.source {
		return
	}
	a.said[tp.says] = place.source
	if tp.Kind == dialect.ToolCall {
		a.emit(ToolCallEvent{ArtifactID: art, CallID: tp.ID, Name: tp.Name, Args: tp.Args})
		if tp.ID != "" {
			a.called[tp.ID] = true
			a.release(tp.ID)
		}
		return
	}
	if tp.ID != "" {
		key := toolKey{place.source, tp.ID}
		if a.snapshot == nil {
			a.live[key] = true
		} else if last, ok := a.snapshot[key]; (ok && last != place) || a.live[key] {
			return
		}
	}
	result := ToolResultEvent{ArtifactID: art, CallID: tp.ID, Name: tp.Name, Response: tp.Result}
	if tp.ID != "" && !a.called[tp.ID] && (a.snapshot != nil || !a.fetchCall(tp.ID)) {
		a.held = append(a.held, result)
		return
	}
	a.emit(result)
}

// seed takes the task an answer is about, as the agent holds it before the answer, and counts every tool part in it as
// delivered: the task goes on, so its final snapshot holds the paused turn's calls and results again, and an answer
// shows its own steps. The task is kept for fetchCall when the answer is a tool approval, whose approved call is a step of
// the answer. A question's call (kagent's ask_user) is not: the paused turn showed it as it asked, and the answer streams
// only its result, which then waits for a call that does not come and is delivered when the send ends.
func (a *assembler) seed(t *a2a.Task, approval bool) {
	if approval {
		a.paused = t
	}
	eachToolPart(t, func(place toolPlace, art a2a.ArtifactID, tp toolPart) {
		if place.source == inArtifact {
			a.sent[artifactPart{art, tp.says}]++
		} else {
			a.placed[place] = true
		}
	})
}

// fetchCall looks in the task the send answers for the call id, once the send has a live result for it and no call,
// and delivers the call if it finds it, then reports whether the call has been delivered. The turn that approves a call
// streams only its result: the call was the paused turn's, and seed has counted it as delivered. Only the call is
// delivered: the rest of the task, such as the error that stood for the pause, is the paused turn's too.
func (a *assembler) fetchCall(id string) bool {
	if a.paused == nil {
		return false
	}
	eachToolPart(a.paused, func(place toolPlace, art a2a.ArtifactID, tp toolPart) {
		if tp.Kind == dialect.ToolCall && tp.ID == id && !a.called[id] {
			a.tool(place, art, tp)
		}
	})
	return a.called[id]
}

// release delivers the results that waited for the call id, or, with no id, all of them.
func (a *assembler) release(id string) {
	waiting := a.held
	a.held = nil
	for _, r := range waiting {
		if id == "" || r.CallID == id {
			a.emit(r)
		} else {
			a.held = append(a.held, r)
		}
	}
}
