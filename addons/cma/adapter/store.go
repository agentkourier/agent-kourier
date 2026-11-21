package adapter

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

// Store is the state the adapter keeps across a restart: the A2A tasks, the message IDs that started or joined them,
// and each context's session. store.Store implements it on SQLite and Postgres.
type Store interface {
	taskstore.Store
	// TasksInStates returns the tasks whose state is one of states.
	TasksInStates(ctx context.Context, states ...a2a.TaskState) ([]*taskstore.StoredTask, error)
	// DeleteSettledBefore deletes the settled tasks last updated before cutoff, with their message IDs.
	DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// MessageTask returns the task a message ID started or joined, if one is recorded.
	MessageTask(ctx context.Context, messageID string) (a2a.TaskID, bool, error)
	// RecordMessage records that a message ID started or joined a task; a recorded ID keeps its task.
	RecordMessage(ctx context.Context, messageID string, taskID a2a.TaskID) error
	// Sessions returns every context's session.
	Sessions(ctx context.Context) ([]store.Session, error)
	// PutSession records a context's session, replacing the one it had.
	PutSession(ctx context.Context, s store.Session) error
	// TouchSession sets the last activity of a context's session while it is still sessionID.
	TouchSession(ctx context.Context, contextID, sessionID string, at time.Time) error
	// ForgetContext removes a context's archived session and its tasks last updated before cutoff.
	ForgetContext(ctx context.Context, contextID, sessionID string, cutoff time.Time) error
}

var _ Store = (*store.Store)(nil)
