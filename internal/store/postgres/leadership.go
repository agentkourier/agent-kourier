package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// The timeouts of fencing (spec D37). They are short because what they bound is a pause in a takeover: the epoch
// check takes a share lock on the one leadership row that a transaction keeps until it ends, and the new leader's
// epoch update cannot go before every such share is gone.
const (
	// writeLockTimeout is how long a write transaction waits for any lock after the epoch check, SET LOCAL for the
	// transaction. A healthy write takes its locks in milliseconds: the others that hold them (the log's, a
	// session's, a limit's) are brief writes. One that waits longer is behind a stalled holder, and the share it
	// holds on the leadership row is what a takeover would wait for, so it gives up, which frees the row, and its
	// caller retries. Three seconds is a few times a slow lock and shorter than the 10 s that a leader is given to
	// renew its lease, so a takeover is never held up by it for more than a few seconds.
	writeLockTimeout = 3 * time.Second
	// takeLockTimeout and takeStatementTimeout bound one attempt of Lead. The attempt waits for the shares of the
	// writers in flight, which end in milliseconds, or in writeLockTimeout for a deposed leader whose write waits on
	// a lock. The lock wait outlasts that: an attempt that timed out first would leave the queue, and the writers
	// queued behind it would go in and could keep it from ever being first. With the longer wait the waiting write
	// gives up and frees its share, and the attempt gets its lock. The statement limit is above the lock's. The
	// broker's retries stay within the renew deadline (10 s).
	takeLockTimeout      = 4 * time.Second
	takeStatementTimeout = 6 * time.Second
)

// Lead takes the next epoch for holder and keeps it: from here the store's writes are checked against it (see
// store.Fenced). It is one attempt, a transaction that updates the one row of leadership under takeLockTimeout and
// takeStatementTimeout; a lock timeout is an error the caller retries. It queues for a lock on the table before it
// updates the row, so that a leader that is deposed but keeps writing cannot hold the takeover back indefinitely.
func (s *adapter) Lead(ctx context.Context, holder string) (int64, error) {
	if holder == "" {
		return 0, fmt.Errorf("lead: %w: the holder is empty", store.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("lead: %w", err)
	}
	var epoch int64
	// Not inTx: its epoch check refuses a store that has no epoch yet, which is what this call is for.
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, setting := range []string{
			fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, takeLockTimeout.Milliseconds()),
			fmt.Sprintf(`SET LOCAL statement_timeout = '%dms'`, takeStatementTimeout.Milliseconds()),
		} {
			if _, err := tx.Exec(ctx, setting); err != nil {
				return fmt.Errorf("set a timeout: %w", err)
			}
		}
		// The table lock first: a row lock alone is starved by a leader that keeps writing, as each of its transactions
		// shares the row and a new share is granted while the update waits. A queued table lock holds the new shares
		// back, so the update waits only for the transactions already in flight. Every check of the epoch (ROW SHARE)
		// conflicts with it, and a plain read (ACCESS SHARE) does not.
		if _, err := tx.Exec(ctx, `LOCK TABLE leadership IN EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("lock the leadership table: %w", err)
		}
		err := tx.QueryRow(ctx, `UPDATE leadership SET epoch = epoch + 1, holder = $1, since = now() RETURNING epoch`, holder).Scan(&epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("the leadership row is missing")
		}
		return err
	})
	if err != nil {
		return 0, s.red.scrub(fmt.Errorf("lead: %w", classify(err)))
	}
	s.epoch.Store(epoch)
	return epoch, nil
}

// checkEpoch is the first statement of every write transaction. It reads the leadership row FOR SHARE, so that the
// new leader's update waits for this transaction to end: a write that passes the check commits before the epoch can
// move, and every write after the move fails the check. A store with no epoch, a standby's, refuses without asking.
func (s *adapter) checkEpoch(ctx context.Context, tx pgx.Tx) error {
	mine := s.epoch.Load()
	if mine == 0 {
		return fmt.Errorf("%w: this store holds no epoch, so it is not the leader", store.ErrDeposed)
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT epoch FROM leadership FOR SHARE`).Scan(&current); err != nil {
		return fmt.Errorf("check the epoch: %w", err)
	}
	if current != mine {
		s.deposed.Inc()
		if s.onDeposed != nil {
			s.onDeposed()
		}
		return fmt.Errorf("%w: epoch %d is stale, the database is at %d", store.ErrDeposed, mine, current)
	}
	return nil
}
