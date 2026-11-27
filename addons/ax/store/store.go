// Package store persists AX launches and their A2A state.
package store

import (
	"log/slog"
	"time"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// Options configures a store.
type Options struct {
	// Now stamps each task's update time and each context's activity. Nil is time.Now.
	Now func() time.Time
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write finds that another replica has taken the lease
	// since this store took it, before the write fails with kitstore.ErrDeposed. It runs in the writer's goroutine and
	// must not call the store. The agent ends its process in it: the error alone would reach whichever caller the write
	// was made for. It does not run for a store that holds no epoch (a standby's, or one that released its lease).
	OnDeposed func()
}

// Store keeps A2A state and fenced AX launches on either supported database.
type Store struct {
	*kitstore.Store
	now func() time.Time
	log *slog.Logger
}

func newStore(db *kitstore.DB, opts Options) *Store {
	s := &Store{Store: kitstore.NewStore(db, kitstore.TaskOptions{Now: opts.Now, ContextActivity: true}),
		now: opts.Now, log: opts.Log}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}
