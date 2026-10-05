package store

import (
	"context"
	"time"
)

// SessionHorizons says, for each Binding, when a session is far enough past its expiry to be deleted. The caller
// owns the policy (a session's threadTTL, which is the Binding's, plus the grace it adds); the store owns what makes a
// session safe to delete.
type SessionHorizons struct {
	// ByBinding maps a Binding's "namespace/name" to a time: a session of that Binding whose last activity is before
	// it is past its horizon.
	ByBinding map[string]time.Time
	// Otherwise is the same for a session of a Binding that ByBinding does not name, which is one that has left the
	// config: its threadTTL is not known. It is required.
	Otherwise time.Time
}

// Valid reports whether every horizon is set: the one for a Binding that is not named, and each one that is.
func (h SessionHorizons) Valid() bool {
	if h.Otherwise.IsZero() {
		return false
	}
	for _, at := range h.ByBinding {
		if at.IsZero() {
			return false
		}
	}
	return true
}

// PurgedSessions counts what one PurgeExpiredSessions call deleted: the sessions, and each kind of row that belonged
// to them.
type PurgedSessions struct {
	Sessions, Replies, Interactions, Presses, Outbox int
}

// Plus returns the sum of p and q, for a caller that purges in batches.
func (p PurgedSessions) Plus(q PurgedSessions) PurgedSessions {
	return PurgedSessions{p.Sessions + q.Sessions, p.Replies + q.Replies, p.Interactions + q.Interactions, p.Presses + q.Presses, p.Outbox + q.Outbox}
}

// SessionRetention deletes sessions that have been expired for good, with what belongs to them. It is a separate
// interface so that nothing which serves a thread can reach it.
type SessionRetention interface {
	// PurgeExpiredSessions deletes at most limit sessions that are past their horizon (see SessionHorizons), the
	// ones last active longest ago first, and every row that belongs to them, in one transaction: their reply queue
	// (the delivered tombstones), their interactions and those interactions' presses, and their outbox rows (a row
	// made for the thread, or one whose source is derived from one of the session's reply IDs as that ID and a "/"
	// and a suffix, as the queued reaction's is: "<reply ID>/queued"). It returns what it deleted;
	// fewer sessions than limit says nothing more is due. A thread whose session is gone is one that courier never
	// owned.
	//
	// A session is never deleted, whatever its age, while it has an active or paused task, a pending interaction, a
	// reply that was never delivered, or an outbox row that was neither posted nor abandoned. It is the first call
	// after what held it back is resolved that deletes it. Only settled rows are deleted (a delivered reply, a decided
	// interaction, a posted or abandoned outbox row), so a row that was written after the sessions were chosen, which
	// no one has settled, survives even if its session does not. Rows that belong to no session (a trigger thread, a
	// trigger run, the audit log, an outbox row of a digest) are not its to delete.
	//
	// Every horizon must be set, and limit positive (ErrInvalid otherwise).
	PurgeExpiredSessions(ctx context.Context, h SessionHorizons, limit int) (PurgedSessions, error)
}
