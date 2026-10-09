package store

import (
	"context"
	"time"
)

// ConversationKey identifies a chat thread: (connection, channel, thread_ts).
type ConversationKey struct {
	Connection string
	Channel    string
	Thread     string
}

// Session maps a conversation to its agent session (spec 9, table sessions).
type Session struct {
	// TraceParent and TraceState preserve W3C ancestry for durable work; never baggage.
	TraceParent, TraceState string
	Key                     ConversationKey
	// ContextID is empty until the agent assigns one in its first response.
	ContextID string
	// TaskID is the active task: the one that runs or waits for input. The session
	// manager clears it when the task ends, so it is empty when no task is active, and
	// Recovery.ListActiveSessions finds the sessions a restart must reconcile by it.
	TaskID string
	// Turn is the turn whose output the active task belongs to, recorded with the task (SetSessionTask), so that a
	// recovery renders the task's output under it and finds the message it began. Empty with no task, and for a task
	// recorded before the column existed. UpsertSession neither writes nor changes it.
	Turn TaskTurn
	// TurnTask is the task Turn was recorded for. A binary from before the turn was recorded starts a task and leaves the
	// turn as it was, so Turn is the active task's only if TurnTask equals TaskID; otherwise it is another task's, and no
	// output of this one belongs to it.
	TurnTask string
	Binding  string
	// Alert is the name of the alert the thread was started for (a trigger's `alertname` entry), or empty for a
	// thread no alert started. It is untrusted text out of a bot's message, kept as the feedback of an investigation's
	// message needs it, and written by UpsertSession alone.
	Alert string
	// Agent is the agent of the Binding when the session was made, namespace/name on its backend, kept so that a vote on a message
	// of the thread names the agent that wrote it, whatever the Binding says later. Empty for a session of a build from before
	// the column. Written by UpsertSession alone.
	Agent        string
	LastActivity time.Time
}

// TaskTurn is the turn whose output a task belongs to: the turn that began it, or for the continuation of a task that was
// paused and answered, the answer. ID names the outbox row of the output (and so the message, a stream's included), and User
// is the platform's ID of the person it is shown to, which a stream needs. The zero value is none.
type TaskTurn struct{ ID, User string }

// Sessions persists the thread-to-session mapping. Key (all three parts),
// Binding and LastActivity are required; ContextID and TaskID are optional.
// A missing required value returns ErrInvalid.
type Sessions interface {
	// UpsertSession inserts s or replaces the row with the same key, every column of it, an empty
	// ContextID or TaskID included. Only the trace context stays when s and the call's context have
	// none: a caller with no trace does not blank the one that another saved.
	UpsertSession(ctx context.Context, s Session) error
	// GetSession returns the session for key. It never writes. ErrNotFound means
	// no row exists. A session whose last activity is more than ttl (spec D28,
	// threadTTL) before now is returned together with an error wrapping
	// ErrExpired, so the caller can tell an expired thread (post the notice)
	// from one Agent Kourier never owned (ignore); a session exactly ttl old is live.
	// ttl must be positive.
	GetSession(ctx context.Context, key ConversationKey, ttl time.Duration, now time.Time) (Session, error)
	// TouchSession sets the session's last activity to at. It returns
	// ErrNotFound if the session does not exist.
	TouchSession(ctx context.Context, key ConversationKey, at time.Time) error
}
