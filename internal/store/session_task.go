package store

import "context"

// SessionTasks records a session's active task without rewriting the row, so it can never
// undo a TouchSession that ran at the same time (UpsertSession replaces every column).
type SessionTasks interface {
	// SetSessionTask sets the session's active task. An empty taskID clears it (the task
	// ended). A non-empty contextID replaces the recorded contextId; an empty one leaves it.
	// Nothing else changes, LastActivity included. It returns ErrNotFound if the session
	// does not exist and ErrInvalid for an incomplete key.
	SetSessionTask(ctx context.Context, key ConversationKey, contextID, taskID string) error
}
