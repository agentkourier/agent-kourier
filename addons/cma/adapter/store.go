package adapter

import (
	"context"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

// Store is what the executor keeps across a restart beside the kit's task state: each context's session.
// store.Store implements it on SQLite and Postgres.
type Store interface {
	// Sessions returns every context's session.
	Sessions(ctx context.Context) ([]store.Session, error)
	// PutSession records a context's session, replacing the one it had.
	PutSession(ctx context.Context, s store.Session) error
	// TouchSession sets the last activity of a context's session while it is still sessionID.
	TouchSession(ctx context.Context, contextID, sessionID string, at time.Time) error
	// ForgetSession removes a context's session that CMA no longer has, and its tasks last updated before cutoff.
	ForgetSession(ctx context.Context, contextID, sessionID string, cutoff time.Time) error
}

var _ Store = (*store.Store)(nil)
