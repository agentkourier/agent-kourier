// Package agent is courier's runtime layer for talking to an A2A agent: it sends a message
// and streams the reply as typed events, reconnects a dropped stream without repeating text,
// and cancels tasks (spec 8.2, 8.3). The session manager drives it; it holds no session state.
package agent

import "github.com/a2aproject/a2a-go/v2/a2a"

// Event is one thing that happened during a send, in the order it happened. The concrete
// types are TextEvent, ToolCallEvent, ToolResultEvent, StateEvent, and FinalEvent.
type Event interface{ isEvent() }

// Handler receives the events of a send. It runs on the goroutine that reads the stream,
// so a slow Handler slows the read; it must not call back into the send.
type Handler func(Event)

// TextEvent reports that an artifact's text changed. Text is everything the artifact holds
// so far, not the new part: a renderer replaces what it shows for ArtifactID. A reconnect
// therefore never repeats text. Artifacts appear in the order of their first TextEvent.
type TextEvent struct {
	ArtifactID a2a.ArtifactID
	Text       string
}

// ToolCallEvent reports that the agent called a tool.
type ToolCallEvent struct {
	ArtifactID a2a.ArtifactID
	CallID     string
	Name       string
	Args       map[string]any
}

// ToolResultEvent reports the outcome of a tool call.
type ToolResultEvent struct {
	ArtifactID a2a.ArtifactID
	CallID     string
	Name       string
	Response   any
}

// StateEvent reports a change of the task's state. A state that repeats is not a change.
type StateEvent struct {
	TaskID    a2a.TaskID
	ContextID string
	State     a2a.TaskState
}

// FinalEvent is the last event of a send that reaches an end state: the task is finished
// (completed, canceled, failed, or rejected) or paused for input. Task holds the agent's
// last word, including the question of an input-required pause.
type FinalEvent struct {
	Task *a2a.Task
}

func (TextEvent) isEvent()       {}
func (ToolCallEvent) isEvent()   {}
func (ToolResultEvent) isEvent() {}
func (StateEvent) isEvent()      {}
func (FinalEvent) isEvent()      {}
