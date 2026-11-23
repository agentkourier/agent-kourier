// Package store keeps the adapter's state where a restart finds it: the kit's task state (the A2A tasks, the message
// IDs that started or joined them) and each A2A context's CMA session, which the kit keeps as the context's ref. It
// runs on SQLite (one file, one adapter) or Postgres (shared by replicas).
//
// The adapter has one caller, Agent Kourier (auth.Caller): the caller check runs every call as it. Tasks and message
// IDs belong to the caller that created them, as the kit keeps them, and a call without an authenticated caller is
// refused; the sessions are auth.Caller's contexts.
//
// The store keeps no tables of its own: the kit's migrations make them all. A database an earlier build of the adapter
// wrote, which kept the sessions in a table of the adapter's (context_sessions) and its migrations in a version table
// (schema_migrations) this build never makes, is refused at the open, by either table, before anything is written
// (ErrEarlierLayout).
package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// ErrEarlierLayout is the open's refusal of a database an earlier build of the adapter wrote: this build cannot read
// its layout, and changes nothing in it. Its text leads with the reason, which a log line's cut of a long error
// (telemetry.MaxLogErrorBytes) keeps.
var ErrEarlierLayout = errors.New("the store holds an earlier adapter's layout, which this version does not read: " +
	"start it on a new store (a new SQLite file, or an empty Postgres database or schema)")

// Session is an A2A context's CMA session.
type Session struct {
	ContextID string
	SessionID string
	// LastActive is when the context was last active: a message for it arrived, or a turn of it started or ended.
	LastActive time.Time
}

// Options configures a store.
type Options struct {
	// Now stamps each task's update time. Nil is time.Now.
	Now func() time.Time
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write finds that another replica has taken the lease since this store took
	// it, before the write fails with kitstore.ErrDeposed. It runs in the writer's goroutine and must not call the
	// store. The adapter ends its process in it: the error alone would reach whichever caller the write was made for.
	// It does not run for a store that holds no epoch (a standby's, or one that released its lease).
	OnDeposed func()
}

// Store is the adapter's state on SQLite or Postgres: the kit's task state, whose database's lease (TryLead, Renew,
// Release, CurrentLeader) fences every write. It implements taskstore.Store. A task's create or update leaves its
// context's activity alone: a context's activity is a message's arrival (TouchContext) and its session's turns
// (PutSession, TouchSession).
type Store struct {
	*kitstore.Store
}

var _ taskstore.Store = (*Store)(nil)

func newStore(db *kitstore.DB, opts Options) *Store {
	return &Store{Store: kitstore.NewStore(db, kitstore.TaskOptions{Now: opts.Now})}
}

// refuseEarlierLayout is the open's kitstore.Options Prepare: it refuses a database that has a table of the earlier
// layout's (its sessions table, or its version table), found by exists, before the kit's migrations write anything.
func refuseEarlierLayout(exists string) func(ctx context.Context, q kitstore.Querier) error {
	return func(ctx context.Context, q kitstore.Querier) error {
		var earlier bool
		if err := q.QueryRow(ctx, exists).Scan(&earlier); err != nil {
			return err
		}
		if earlier {
			return ErrEarlierLayout
		}
		return nil
	}
}

// Sessions returns every context's session.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	all, err := s.Contexts(ctx)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, c := range all {
		if c.Owner == auth.Caller && c.Ref != "" {
			out = append(out, Session{ContextID: c.ContextID, SessionID: c.Ref, LastActive: c.LastActive})
		}
	}
	return out, nil
}

// PutSession records the context's session, replacing the one it had.
func (s *Store) PutSession(ctx context.Context, ss Session) error {
	return s.PutContext(ctx, kitstore.Context{Owner: auth.Caller, ContextID: ss.ContextID, LastActive: ss.LastActive, Ref: ss.SessionID})
}

// TouchSession sets the last activity of the context's session, while that session is still sessionID.
func (s *Store) TouchSession(ctx context.Context, contextID, sessionID string, at time.Time) error {
	return s.TouchContextRef(ctx, auth.Caller, contextID, sessionID, at)
}

// ForgetSession is for a context whose session, sessionID, CMA no longer has. It removes that session, if the context
// still has it, and deletes the context's tasks last updated before cutoff, with their message IDs: they belong to the
// lost session and can never run again. A task the context started after cutoff, in a new session, is kept. It is
// the kit's ForgetContext for the adapter's one caller, which the kit's upkeep calls itself once the adapter has
// archived an idle context's session.
func (s *Store) ForgetSession(ctx context.Context, contextID, sessionID string, cutoff time.Time) error {
	return s.ForgetContext(ctx, auth.Caller, contextID, sessionID, cutoff)
}
