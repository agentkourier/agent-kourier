package agent

import (
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// kagent marks each tool call and tool result artifact with a part-type in the metadata of
// its data part (WP-02, measured). The convention belongs to the kagent-v1 dialect; a
// generic a2a dialect would need its own.
const (
	partTypeKey          = "kagent.dev/a2a/part-type"
	partFunctionCall     = "function_call"
	partFunctionResponse = "function_response"
)

// assembler folds the events of one send, and of every reconnect within it, into Events.
// A task snapshot (from GetTask, SubscribeToTask, or a replayed send) and a live chunk
// take the same path, so the same text never arrives twice: an artifact chunk with
// append: true adds to the artifact's text, and anything else replaces it (spec 8.3).
type assembler struct {
	emit      Handler
	text      map[a2a.ArtifactID]string
	tools     map[string]bool // "artifact/part" of the tool parts already delivered
	taskID    a2a.TaskID
	contextID string
	state     a2a.TaskState
	final     *a2a.Task
}

func newAssembler(h Handler) *assembler {
	if h == nil {
		h = func(Event) {}
	}
	return &assembler{emit: h, text: map[a2a.ArtifactID]string{}, tools: map[string]bool{}}
}

// apply folds one event from the agent into the assembler and emits what changed.
func (a *assembler) apply(ev a2a.Event) {
	switch e := ev.(type) {
	case *a2a.Task:
		a.track(e.ID, e.ContextID)
		for _, art := range e.Artifacts {
			a.artifact(art, false)
		}
		a.setState(e.Status.State)
		if settled(e.Status.State) {
			a.final = e
			a.emit(FinalEvent{Task: e})
		}
	case *a2a.TaskStatusUpdateEvent:
		a.track(e.TaskID, e.ContextID)
		a.setState(e.Status.State)
	case *a2a.TaskArtifactUpdateEvent:
		a.track(e.TaskID, e.ContextID)
		a.artifact(e.Artifact, e.Append)
	}
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
// and its tool parts once each.
func (a *assembler) artifact(art *a2a.Artifact, appendMode bool) {
	if art == nil {
		return
	}
	var text strings.Builder
	hasText := false
	for i, p := range art.Parts {
		switch c := p.Content.(type) {
		case a2a.Text:
			text.WriteString(string(c))
			hasText = true
		case a2a.Data:
			a.tool(art.ID, i, p, c)
		}
	}
	if !hasText {
		return
	}
	old, t := a.text[art.ID], text.String()
	if appendMode {
		t = old + t
	}
	if t != old {
		a.text[art.ID] = t
		a.emit(TextEvent{ArtifactID: art.ID, Text: t})
	}
}

func (a *assembler) tool(art a2a.ArtifactID, i int, p *a2a.Part, d a2a.Data) {
	key := string(art) + "/" + strconv.Itoa(i)
	if a.tools[key] {
		return
	}
	m, _ := d.Value.(map[string]any)
	id, _ := m["id"].(string)
	name, _ := m["name"].(string)
	switch p.Metadata[partTypeKey] {
	case partFunctionCall:
		args, _ := m["args"].(map[string]any)
		a.emit(ToolCallEvent{ArtifactID: art, CallID: id, Name: name, Args: args})
	case partFunctionResponse:
		a.emit(ToolResultEvent{ArtifactID: art, CallID: id, Name: name, Response: m["response"]})
	default:
		return
	}
	a.tools[key] = true
}
