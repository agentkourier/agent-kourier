package adapter

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// Store is the state the adapter keeps across a restart: the A2A tasks, the message IDs that started or joined them,
// and each context's session. store.Store implements it on SQLite and Postgres.
type Store interface {
	taskstore.Store
	// SettleInterrupted fails every task a restart left submitted or working, with an agent message of text, and
	// says which it failed: one whose record does not decode by its state alone, and one that changed since it was
	// read it logs and leaves.
	SettleInterrupted(ctx context.Context, text string) (kitstore.Settlement, error)
	// DeleteSettledBefore deletes the settled tasks last updated before cutoff, with their message IDs.
	DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// MessageTask returns the call's caller's task that a message ID started or joined, or nil when none is recorded.
	// The store records a task's message IDs when it stores the task.
	MessageTask(ctx context.Context, messageID string) (*a2a.Task, error)
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
