// Package store keeps the agent's state where a restart finds it: the kit's task state (the A2A tasks, the message IDs
// that started or joined them, and when each A2A context was last active), and ADK's sessions and their events, which
// ADK writes through gorm on the same database. It runs on SQLite (one file, one agent) or Postgres (shared by
// replicas).
//
// Tasks and message IDs belong to the caller that created them (package auth): every read and write made for an A2A
// call is limited to the call's caller, and one without an authenticated caller is refused. The upkeep (SettleInterrupted,
// DeleteSettledBefore, DeleteIdleContexts) works across callers.
package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/session"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// AppName is the ADK app the agent runs its sessions under; the store deletes an idle context's session of it.
const AppName = "agent-kourier-ax"

// Options configures a store.
type Options struct {
	// Now stamps each task's update time and each context's activity. Nil is time.Now.
	Now func() time.Time
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write, the store's or ADK's, finds that another replica has taken the lease
	// since this store took it, before the write fails with kitstore.ErrDeposed. It runs in the writer's goroutine and
	// must not call the store. The agent ends its process in it: the error alone would reach whichever caller the write
	// was made for. It does not run for a store that holds no epoch (a standby's, or one that released its lease).
	OnDeposed func()
}

// Store is the agent's state on SQLite or Postgres: the kit's task state, whose database's lease (TryLead, Renew,
// Release, CurrentLeader) fences every write, the store's and ADK's, and ADK's session service on the same database
// (Sessions). It implements taskstore.Store. A task's create or update marks its context active, and DeleteIdleContexts
// deletes an idle context's ADK session with its tasks.
type Store struct {
	*kitstore.Store
	now func() time.Time
	log *slog.Logger

	sessions session.Service
}

var _ taskstore.Store = (*Store)(nil)

func newStore(db *kitstore.DB, opts Options) *Store {
	s := &Store{Store: kitstore.NewStore(db, kitstore.TaskOptions{Now: opts.Now, ContextActivity: true, ReleaseContext: deleteADKSession}),
		now: opts.Now, log: opts.Log}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Sessions is ADK's session service, on the store's database.
func (s *Store) Sessions() session.Service { return s.sessions }

// deleteADKSession deletes the ADK session of an idle context, and the session's events, in the transaction that
// deletes the context: ADK keys a context's session by the context's owner and ID.
func deleteADKSession(ctx context.Context, q kitstore.Querier, c kitstore.Context) error {
	for _, stmt := range []string{
		`DELETE FROM events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		`DELETE FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?`,
	} {
		if _, err := q.Exec(ctx, stmt, AppName, c.Owner, c.ContextID); err != nil {
			return err
		}
	}
	return nil
}
