package store

import "context"

// Recovery lists what a restart has to reconcile (spec 8.2, 8.3). Both methods only read.
// They never write, and never change a session's LastActivity, so listing cannot revive an
// expired session.
type Recovery interface {
	// ListActiveSessions returns every session that has an active task (TaskID set), oldest
	// last activity first (sessions with the same activity in any order). Expired sessions are
	// included: the caller reads each with GetSession and decides.
	ListActiveSessions(ctx context.Context) ([]Session, error)
	// ListQueuedSessions returns the key of every session with at least one undelivered
	// reply, once each, in any order.
	ListQueuedSessions(ctx context.Context) ([]ConversationKey, error)
}
