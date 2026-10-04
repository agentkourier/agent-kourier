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
	TaskID       string
	Binding      string
	LastActivity time.Time
}

// Sessions persists the thread-to-session mapping. Key (all three parts),
// Binding and LastActivity are required; ContextID and TaskID are optional.
// A missing required value returns ErrInvalid.
type Sessions interface {
	// UpsertSession inserts s or replaces the row with the same key.
	UpsertSession(ctx context.Context, s Session) error
	// GetSession returns the session for key. It never writes. ErrNotFound means
	// no row exists. A session whose last activity is more than ttl (spec D28,
	// threadTTL) before now is returned together with an error wrapping
	// ErrExpired, so the caller can tell an expired thread (post the notice)
	// from one courier never owned (ignore); a session exactly ttl old is live.
	// ttl must be positive.
	GetSession(ctx context.Context, key ConversationKey, ttl time.Duration, now time.Time) (Session, error)
	// TouchSession sets the session's last activity to at. It returns
	// ErrNotFound if the session does not exist.
	TouchSession(ctx context.Context, key ConversationKey, at time.Time) error
}
