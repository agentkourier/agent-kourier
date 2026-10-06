package store

import (
	"context"
	"time"
)

// StaleTask is a session whose recorded task has outlived the session's horizon (see SessionHorizons): the session was
// last active before it, and its task_id is still set, which only a crash that came after the session expired leaves
// (recovery skips an expired session, and the session manager clears a task as it ends).
type StaleTask struct {
	Key ConversationKey
	// Binding is the session's Binding, "namespace/name", which may have left the config.
	Binding string
	// ContextID is the agent's context of the session, for the audit; it may be empty.
	ContextID string
	// TaskID is the recorded task.
	TaskID string
}

// EndedStaleTask is what EndStaleTask did.
type EndedStaleTask struct {
	// Ended is true when the session's task was cleared. It is false when the session was no longer a stale task's:
	// it is gone, its task is another or none, or it has been active since, and nothing was changed.
	Ended bool
	// Interactions is how many pending interactions of the task were cancelled with it.
	Interactions int
}

// StaleTasks ends the tasks that a crash left on sessions that are past their horizon, so that the sweep that deletes
// such sessions (SessionRetention) is not held back by them for ever. It is a separate interface so that nothing which
// serves a thread can reach it.
type StaleTasks interface {
	// ListStaleTasks returns at most limit sessions that are past their horizon (SessionHorizons) and record a task,
	// the ones last active longest ago first. It reads, and changes nothing: a session inside its horizon is never
	// listed, whatever it records. Every horizon must be set, and limit positive (ErrInvalid otherwise).
	ListStaleTasks(ctx context.Context, h SessionHorizons, limit int) ([]StaleTask, error)
	// EndStaleTask clears the task that t records, with the turn recorded for it (the context stays), and cancels the
	// pending interactions of that task (those of the session that name it), recording now as when they were decided,
	// in one transaction. It is a compare-and-set against what ListStaleTasks saw: it changes nothing, and reports
	// Ended false, unless the session still records t.TaskID and is still past its horizon h. LastActivity is not
	// touched. Every horizon must be set, now non-zero, and the key and the task complete (ErrInvalid otherwise).
	EndStaleTask(ctx context.Context, h SessionHorizons, t StaleTask, now time.Time) (EndedStaleTask, error)
}
