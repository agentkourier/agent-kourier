package store

import "context"

// SessionTasks records a session's active task without rewriting the row, so it can never
// undo a TouchSession that ran at the same time (UpsertSession replaces every column).
type SessionTasks interface {
	// SetSessionTask sets the session's active task and the turn its output belongs to, in one write: an empty taskID
	// clears the task (it ended), and the turn with it, whatever turn is given. A non-empty contextID replaces the recorded
	// contextId; an empty one leaves it. A later call replaces the turn, so a task that is answered records the answer's; the task it was recorded for is
	// kept with it (Session.TurnTask).
	// Nothing else changes, LastActivity included, except that a call made inside a trace records
	// that trace as the session's (TraceParent and TraceState), and one made outside any leaves it. It
	// returns ErrNotFound if the session does not exist and ErrInvalid for an incomplete key.
	SetSessionTask(ctx context.Context, key ConversationKey, contextID, taskID string, turn TaskTurn) error
	// ClearSessionAcknowledgment clears only the saved turn's acknowledgment fields, and only while taskID and turnID
	// still identify the session's active task and turn. A missing or different task/turn is a harmless no-op, so this
	// cleanup cannot restore or clear metadata that ResetSession or a newer turn already changed.
	ClearSessionAcknowledgment(ctx context.Context, key ConversationKey, taskID, turnID string) error
	// ResetSession forgets the agent's session: it clears the contextId, the active
	// task and its turn in one UPDATE, and leaves the Binding and LastActivity alone, so the thread stays
	// owned and its next turn starts a fresh context. SetSessionTask cannot do this: it never
	// clears a contextId. Errors are as for SetSessionTask.
	ResetSession(ctx context.Context, key ConversationKey) error
}
