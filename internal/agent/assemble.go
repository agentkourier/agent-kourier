package agent

import (
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// ToolClassifier tells a tool call or tool result part from other content. It is the
// dialect's ToolPart method: what marks a tool part is the backend's convention, not A2A's.
// With no classifier, data parts are opaque and no tool event is delivered.
type ToolClassifier func(part *a2a.Part) (dialect.ToolPart, bool)

// assembler folds the events of one send, and of every reconnect within it, into Events.
// A task snapshot (from GetTask, SubscribeToTask, or a replayed send) and a live chunk
// take the same path, so the same text never arrives twice: an artifact chunk with
// append: true adds to the artifact's text, and anything else replaces it (spec 8.3).
type assembler struct {
	emit      Handler
	classify  ToolClassifier
	text      map[a2a.ArtifactID]string
	tools     map[string]bool // "artifact/part" of the tool parts already delivered
	taskID    a2a.TaskID
	contextID string
	state     a2a.TaskState
	final     *a2a.Task
	changes   int  // events emitted so far: text, tool, state, and final
	replies   bool // a task-less agent Message may be the reply: set for a Send, never for an Answer or a follow
}

func newAssembler(h Handler, classify ToolClassifier) *assembler {
	if h == nil {
		h = func(Event) {}
	}
	a := &assembler{classify: classify, text: map[a2a.ArtifactID]string{}, tools: map[string]bool{}}
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
		for _, art := range e.Artifacts {
			a.artifact(art, false, true)
		}
		a.setState(e.Status.State)
		if settled(e.Status.State) && a.final == nil {
			a.final = e
			a.emit(FinalEvent{Task: e})
		}
	case *a2a.TaskStatusUpdateEvent:
		a.track(e.TaskID, e.ContextID)
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
// the turn), and one after the agent has named a task.
func (a *assembler) message(m *a2a.Message) {
	if !a.replies || m.TaskID != "" || m.Role != a2a.MessageRoleAgent || a.taskID != "" || a.final != nil {
		return
	}
	a.track("", m.ContextID)
	a.setState(a2a.TaskStateCompleted)
	a.artifact(&a2a.Artifact{ID: a2a.ArtifactID(m.ID), Parts: m.Parts}, false, false)
	a.final = &a2a.Task{ContextID: a.contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: m}}
	a.emit(FinalEvent{Task: a.final})
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
		if tp, ok := a.toolPart(p); ok {
			a.tool(art.ID, i, tp)
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

func (a *assembler) toolPart(p *a2a.Part) (dialect.ToolPart, bool) {
	if a.classify == nil {
		return dialect.ToolPart{}, false
	}
	return a.classify(p)
}

// tool delivers a tool part once, however often a snapshot repeats it.
func (a *assembler) tool(art a2a.ArtifactID, i int, tp dialect.ToolPart) {
	key := string(art) + "/" + strconv.Itoa(i)
	if a.tools[key] {
		return
	}
	a.tools[key] = true
	if tp.Kind == dialect.ToolCall {
		a.emit(ToolCallEvent{ArtifactID: art, CallID: tp.ID, Name: tp.Name, Args: tp.Args})
	} else {
		a.emit(ToolResultEvent{ArtifactID: art, CallID: tp.ID, Name: tp.Name, Response: tp.Result})
	}
}
