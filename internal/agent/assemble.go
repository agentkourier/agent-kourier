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
	placed    map[toolPlace]bool               // the places whose tool part was looked at
	said      map[[sha256.Size]byte]toolSource // what each tool part delivered or held said, and where it was found
	called    map[string]bool                  // the call IDs whose call was delivered
	live      map[toolKey]bool                 // the calls, as a source reports them, that had a result from a live event
	held      []ToolResultEvent                // results whose call has not come, in the order they came
	snapshot  map[toolKey]toolPlace            // while a task snapshot is applied: the place of the last result of each call; else nil
	taskID    a2a.TaskID
	contextID string
	state     a2a.TaskState
	final     *a2a.Task
	changes   int  // events emitted so far: text, tool, state, and final
	replies   bool // a task-less agent Message may be the reply: set for a Send, never for an Answer or a follow
	unshown   bool // an agent Message came before any task where it cannot be the reply: on an Answer
}

func newAssembler(h Handler) *assembler {
	if h == nil {
		h = func(Event) {}
	}
	a := &assembler{text: map[a2a.ArtifactID]string{}, placed: map[toolPlace]bool{}, said: map[[sha256.Size]byte]toolSource{},
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
		for _, art := range e.Artifacts {
			a.artifact(art, false, true)
		}
		a.statusMessage(e.Status.State, e.Status.Message)
		a.snapshot = nil
		a.setState(e.Status.State)
		if settled(e.Status.State) && a.final == nil {
			a.end(e)
		}
	case *a2a.TaskStatusUpdateEvent:
		a.track(e.TaskID, e.ContextID)
		a.statusMessage(e.Status.State, e.Status.Message)
		a.setState(e.Status.State)
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
	a.emit(FinalEvent{Task: task})
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
	for i, p := range art.Parts {
		if tp, ok := dialect.DecodeToolPart(p); ok {
			a.tool(artifactPlace(art.ID, i), art.ID, tp)
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

// readable says whether a status message is read for tool parts: the agent's own message while the task works, which
// is where kagent 0.9.x and Google ADK's older A2A converter send tool calls and results
// (test/fixtures/kagent/v09-tools). The user's message echoed back in a status update is not the agent's, and the
// message of a pause or of the end of the turn is the question or the answer, not progress. The message's text is
// not read: 0.9.x repeats there the answer that its artifact holds.
func readable(state a2a.TaskState, m *a2a.Message) bool {
	return m != nil && m.Role == a2a.MessageRoleAgent && state == a2a.TaskStateWorking
}

// statusMessage delivers the tool parts of a status message, a status update's or a task snapshot's, if it is readable.
func (a *assembler) statusMessage(state a2a.TaskState, m *a2a.Message) {
	if !readable(state, m) {
		return
	}
	for i, p := range m.Parts {
		if tp, ok := dialect.DecodeToolPart(p); ok {
			a.tool(messagePlace(m, i, tp), "", tp)
		}
	}
}

// toolSource is where a tool part was found: the two places an agent puts one.
type toolSource uint8

const (
	inArtifact toolSource = iota
	inMessage             // the message of a status update, or of a task snapshot's status
)

// toolPlace is where a tool part sits within its source, which is its identity there.
type toolPlace struct {
	source toolSource
	owner  string // the artifact's ID, or the message's
	index  int    // the part's index in the artifact or message
	// said is the digest of what the part says, set for a message with no ID, which has no place but its words: two
	// such messages are one only if their parts say the same. The zero value says no words.
	said [sha256.Size]byte
}

// toolKey names the call that a result answers, as one source reports it.
type toolKey struct {
	source toolSource
	id     string
}

func artifactPlace(id a2a.ArtifactID, index int) toolPlace {
	return toolPlace{source: inArtifact, owner: string(id), index: index}
}

func messagePlace(m *a2a.Message, index int, tp dialect.ToolPart) toolPlace {
	if m.ID == "" {
		return toolPlace{source: inMessage, index: index, said: saying(tp)}
	}
	return toolPlace{source: inMessage, owner: m.ID, index: index}
}

// saying is a digest of what a tool part says: its kind, call ID and name, and its arguments or response. A digest,
// so that the assembler keeps 32 bytes of each part for the whole send and not its arguments and response.
func saying(tp dialect.ToolPart) [sha256.Size]byte {
	b, err := json.Marshal([]any{tp.Kind, tp.ID, tp.Name, tp.Args, tp.Result})
	if err != nil { // unreachable for a part decoded from JSON
		b = fmt.Appendf(nil, "%#v", tp)
	}
	return sha256.Sum256(b)
}

// lastResults is the place of the last result of each call that a task snapshot holds, in its artifacts and in its
// status message, as a source orders them: its artifacts in the order they came, and the status message after them.
func lastResults(t *a2a.Task) map[toolKey]toolPlace {
	last := map[toolKey]toolPlace{}
	note := func(place toolPlace, p *a2a.Part) {
		if tp, ok := dialect.DecodeToolPart(p); ok && tp.Kind == dialect.ToolResult && tp.ID != "" {
			last[toolKey{place.source, tp.ID}] = place
		}
	}
	for _, art := range t.Artifacts {
		if art == nil {
			continue
		}
		for i, p := range art.Parts {
			note(artifactPlace(art.ID, i), p)
		}
	}
	if m := t.Status.Message; readable(t.Status.State, m) {
		for i, p := range m.Parts {
			if tp, ok := dialect.DecodeToolPart(p); ok {
				note(messagePlace(m, i, tp), p)
			}
		}
	}
	return last
}

// tool delivers one tool part, art being its artifact if it came in one. Each part is delivered once, however often a
// snapshot or a status update repeats it, and its meaning is kept:
//
//   - Within one source, the part's place is its identity. kagent 1.0 sends two results for one call ID, as two
//     artifacts, when a call is approved (test/fixtures/kagent/approval), and both are the agent's account.
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
//   - A result is delivered after its call: one that comes first waits for the call, as the approved call's result
//     does until that replay brings the call, and is delivered at the end of the send if the call never comes.
func (a *assembler) tool(place toolPlace, art a2a.ArtifactID, tp dialect.ToolPart) {
	if a.placed[place] {
		return
	}
	a.placed[place] = true
	says := saying(tp)
	if first, seen := a.said[says]; seen && first != place.source {
		return
	}
	a.said[says] = place.source
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
	if tp.ID != "" && !a.called[tp.ID] {
		a.held = append(a.held, result)
		return
	}
	a.emit(result)
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
