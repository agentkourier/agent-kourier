package store

import "context"

// Fenced is implemented by a store that admits the writes of one leader at a time (spec 11, D37): the Postgres
// store, which more than one replica shares. The SQLite store is not Fenced: one replica opens its file, so there
// is no other leader to fence off, and it ignores epochs.
//
// A Fenced store refuses every write (ErrDeposed) until Lead has been called, and again once another replica's Lead
// has made its epoch stale. Reads are never fenced.
type Fenced interface {
	// Lead makes this store the leader's: it takes the next epoch for holder, a name that says who leads, and
	// returns it. From the moment it returns, a write of this store succeeds only while no other replica has led
	// since, and a write of every other store fails. One call is one attempt, bounded by a short lock and statement
	// timeout of its own, so that a deposed leader's writes that wait on a lock cannot hold the takeover up: the
	// caller retries it while it still holds the lease.
	Lead(ctx context.Context, holder string) (epoch int64, err error)
}
