package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDeposed is a write refused because this database does not hold the lease: another replica has taken it since, or
// this one never took it or has released it.
var ErrDeposed = errors.New("this replica does not hold the lease")

// ErrNoElection is TryLead, Renew, Release or CurrentLeader on SQLite, which is one replica's store and elects no
// leader.
var ErrNoElection = errors.New("the SQLite store elects no leader")

// The timeouts of the election, which bound a pause in a takeover. A takeover locks the leadership table, which waits
// for the write transactions that passed their epoch check; a writer that then waits for some other lock gives up after
// LeaseWriteLockTimeout and frees the table, so a takeover attempt is never held up for longer than
// LeaseTakeLockTimeout (they are the broker's values, internal/store/postgres/leadership.go).
const (
	// LeaseWriteLockTimeout is how long a write, once past its epoch check, waits for a lock.
	LeaseWriteLockTimeout = 3 * time.Second
	// LeaseTakeLockTimeout is how long an attempt to take the lease waits for a lock.
	LeaseTakeLockTimeout = 4 * time.Second
	// LeaseTakeStatementTimeout is how long each statement of an attempt to take the lease runs.
	LeaseTakeStatementTimeout = 6 * time.Second
)

// Lease is the leadership row as a replica read it.
type Lease struct {
	// Holder names the replica that took the lease last (POD_NAME).
	Holder string
	// Address is the holder's ADVERTISE_URL, where a standby forwards A2A requests.
	Address string
	// Epoch counts the takes: each one adds one, and a write checks it.
	Epoch int64
	// Live is whether the lease had not expired when it was read, by the database's clock.
	Live bool
}

// TryLead makes one attempt to take the lease for holder, whose ADVERTISE_URL is address, for d. It takes it when the
// lease has expired or holder already holds it (a process that restarted in its pod: the one that held it is gone),
// and a take moves the epoch, which fences every write of the database that held it before, the fenced ones of a
// library (ADK's) included. ok reports a take; either way lease is the row as it stands after the attempt. A live lease
// of another holder is read without a lock, so a standby's attempts never hold up the leader's writes.
func (db *DB) TryLead(ctx context.Context, holder, address string, d time.Duration) (lease Lease, ok bool, err error) {
	if !db.elects {
		return Lease{}, false, ErrNoElection
	}
	if holder == "" {
		return Lease{}, false, errors.New("take the lease: the holder is empty")
	}
	if lease, err = db.CurrentLeader(ctx); err != nil {
		return Lease{}, false, err
	}
	if lease.Live && lease.Holder != holder {
		return lease, false, nil
	}
	var epoch int64
	err = db.conn.inTx(ctx, func(q Querier) error {
		for _, setting := range []string{
			fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, LeaseTakeLockTimeout.Milliseconds()),
			fmt.Sprintf(`SET LOCAL statement_timeout = '%dms'`, LeaseTakeStatementTimeout.Milliseconds()),
		} {
			if _, err := q.Exec(ctx, setting); err != nil {
				return fmt.Errorf("set a timeout: %w", err)
			}
		}
		// The table lock conflicts with the row lock of every epoch check: it waits for the writes in flight, which
		// commit under the old epoch, and holds back the ones that start after it, which then find the new one. It
		// queues ahead of those, so a leader that keeps writing cannot starve it. A plain read does not conflict.
		if _, err := q.Exec(ctx, `LOCK TABLE leadership IN EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("lock the leadership table: %w", err)
		}
		err := q.QueryRow(ctx, `UPDATE leadership SET epoch = epoch + 1, holder = ?, address = ?,
			expires_at = now() + make_interval(secs => ?), since = now()
			WHERE expires_at <= now() OR holder = ? RETURNING epoch`, holder, address, d.Seconds(), holder).Scan(&epoch)
		if errors.Is(err, ErrNoRows) {
			return nil // another replica took it between the read and the lock
		}
		return err
	})
	if err != nil {
		return Lease{}, false, fmt.Errorf("take the lease: %w", err)
	}
	if epoch == 0 {
		lease, err = db.CurrentLeader(ctx)
		return lease, false, err
	}
	db.epoch.Store(epoch)
	return Lease{Holder: holder, Address: address, Epoch: epoch, Live: true}, true, nil
}

// Renew extends the lease this database holds to d from now, by the database's clock. It fails with ErrDeposed when
// the database does not hold the current epoch. It takes no lock that a write's epoch check holds, so it never waits
// for one.
func (db *DB) Renew(ctx context.Context, d time.Duration) error {
	if !db.elects {
		return ErrNoElection
	}
	mine := db.epoch.Load()
	if mine == 0 {
		return fmt.Errorf("renew the lease: %w", ErrDeposed)
	}
	n, err := db.conn.reader().Exec(ctx, `UPDATE leadership SET expires_at = now() + make_interval(secs => ?) WHERE epoch = ?`, d.Seconds(), mine)
	if err != nil {
		return fmt.Errorf("renew the lease: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("renew the lease: epoch %d is stale: %w", mine, ErrDeposed)
	}
	return nil
}

// Release ends the lease this database holds now, so that a standby takes it on its next attempt instead of after the
// lease's duration, and gives up the database's epoch: its writes fail with ErrDeposed from here, without OnDeposed. A
// database that does not hold the current epoch changes nothing.
func (db *DB) Release(ctx context.Context) error {
	if !db.elects {
		return ErrNoElection
	}
	mine := db.epoch.Swap(0)
	if mine == 0 {
		return nil
	}
	if _, err := db.conn.reader().Exec(ctx, `UPDATE leadership SET expires_at = now() WHERE epoch = ?`, mine); err != nil {
		return fmt.Errorf("release the lease: %w", err)
	}
	return nil
}

// CurrentLeader reads the lease without a lock.
func (db *DB) CurrentLeader(ctx context.Context) (Lease, error) {
	if !db.elects {
		return Lease{}, ErrNoElection
	}
	var l Lease
	err := db.conn.reader().QueryRow(ctx, `SELECT holder, address, epoch, expires_at > now() FROM leadership`).
		Scan(&l.Holder, &l.Address, &l.Epoch, &l.Live)
	if err != nil {
		return Lease{}, fmt.Errorf("read the lease: %w", err)
	}
	return l, nil
}

// checkEpoch is the fence of a Postgres database: the first statement of every write transaction, Write's and the
// fenced ones of a library (ADK's, whose gorm callback runs Fence in its transaction). It reads the leadership row FOR
// KEY SHARE, which a takeover's table lock waits for, so a write that passes the check commits before the epoch can
// move, and every write after the move fails it. KEY SHARE, not SHARE (the broker's), because it does not conflict with
// a renewal's update of expires_at: the leader's renewal never waits for its own writes. A database that holds no epoch
// (a standby, or after Release) refuses without asking, and without OnDeposed.
func (db *DB) checkEpoch(ctx context.Context, q Querier) error {
	mine := db.epoch.Load()
	if mine == 0 {
		return ErrDeposed
	}
	var current int64
	if err := q.QueryRow(ctx, `SELECT epoch FROM leadership FOR KEY SHARE`).Scan(&current); err != nil {
		return fmt.Errorf("check the epoch: %w", err)
	}
	if current != mine {
		if db.onDeposed != nil {
			db.onDeposed()
		}
		return fmt.Errorf("epoch %d is stale, the database is at %d: %w", mine, current, ErrDeposed)
	}
	// Past the check this transaction holds the row, which a takeover waits for: a lock it waits on for longer than a
	// healthy write would is given up on, which frees the row.
	if _, err := q.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, LeaseWriteLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	return nil
}
