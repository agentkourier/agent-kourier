// Package adapter serves an A2A agent backed by a Claude Managed Agents (CMA) agent, so that an A2A client such as
// Agent Kourier can drive it. One A2A context is one CMA session; one A2A task is one turn of that session.
package adapter

import (
	"context"
	"encoding/json"
	"time"
)

// Backend is the part of the Managed Agents API the adapter uses. SDK implements it over the Anthropic Go SDK; tests
// use a fake.
type Backend interface {
	// CreateSession starts a session of the configured agent in the configured environment and returns its ID.
	CreateSession(ctx context.Context, title string, metadata map[string]string) (string, error)
	// Stream opens the session's live event stream. It delivers only events that happen after it opens, so the
	// adapter opens it before it sends anything.
	Stream(ctx context.Context, sessionID string) (EventStream, error)
	// Send sends client events to the session.
	Send(ctx context.Context, sessionID string, events ...Outbound) error
	// Sessions lists the configured agent's sessions in the configured environment that are not archived and carry
	// an A2A context ID (metadata contextMetadataKey): the ones this adapter made.
	Sessions(ctx context.Context) ([]SessionInfo, error)
	// Archive makes the session read-only and releases its sandbox. It cannot be undone.
	Archive(ctx context.Context, sessionID string) error
}

// contextMetadataKey is the session metadata key that holds the A2A context ID, so a restarted adapter can find each
// context's session again.
const contextMetadataKey = "a2a_context_id"

// SessionInfo is what the adapter reads of an existing session.
type SessionInfo struct {
	ID         string
	ContextID  string
	LastActive time.Time
}

// EventStream is a session's live event stream.
type EventStream interface {
	// Next blocks for the next event and reports whether there is one.
	Next() bool
	// Current is the event Next found.
	Current() Event
	// Err is the error that ended the stream, or nil for a clean end.
	Err() error
	// Close releases the stream.
	Close() error
}

// Event is the subset of a session event the adapter reads, decoded from the event's JSON.
type Event struct {
	ID                  string          `json:"id"`
	Type                string          `json:"type"`
	ProcessedAt         *time.Time      `json:"processed_at"`
	SessionThreadID     string          `json:"session_thread_id"`
	Content             []ContentBlock  `json:"content"`
	Name                string          `json:"name"`
	Input               json.RawMessage `json:"input"`
	EvaluatedPermission string          `json:"evaluated_permission"`
	ToolUseID           string          `json:"tool_use_id"`
	MCPToolUseID        string          `json:"mcp_tool_use_id"`
	IsError             bool            `json:"is_error"`
	StopReason          *StopReason     `json:"stop_reason"`
	Error               *EventError     `json:"error"`
}

// ContentBlock is one block of an event's content. Only text is read; other blocks are counted, not shown.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// StopReason says why a session went idle.
type StopReason struct {
	Type     string   `json:"type"`
	EventIDs []string `json:"event_ids"`
}

// EventError is a session.error's error.
type EventError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// The event types the adapter acts on.
const (
	evUserMessage       = "user.message"
	evAgentMessage      = "agent.message"
	evAgentToolUse      = "agent.tool_use"
	evAgentToolResult   = "agent.tool_result"
	evUserToolResult    = "user.tool_result" // a self-hosted worker's result of a built-in tool
	evAgentMCPToolUse   = "agent.mcp_tool_use"
	evAgentMCPResult    = "agent.mcp_tool_result"
	evAgentCustomTool   = "agent.custom_tool_use"
	evSessionError      = "session.error"
	evStatusRunning     = "session.status_running"
	evStatusIdle        = "session.status_idle"
	evStatusTerminated  = "session.status_terminated"
	stopEndTurn         = "end_turn"
	stopRequiresAction  = "requires_action"
	permissionAsk       = "ask"
	permissionDeny      = "deny"
	maxToolResultLength = 4000
)

// Outbound is a client event the adapter sends. Exactly one field is set.
type Outbound struct {
	// Message is the text of a user.message.
	Message *string
	// Confirmation answers a tool call that waits on a person.
	Confirmation *Confirmation
	// Interrupt stops the running turn.
	Interrupt bool
}

// Confirmation is a user.tool_confirmation.
type Confirmation struct {
	ToolUseID   string // the agent.tool_use or agent.mcp_tool_use event's ID
	Allow       bool
	DenyMessage string // only with Allow false; empty sends none
}

// UserMessage returns the Outbound of a user.message.
func UserMessage(text string) Outbound { return Outbound{Message: &text} }
