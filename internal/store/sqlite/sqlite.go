// Package sqlite implements store.Store on SQLite using the pure-Go
// modernc.org/sqlite driver, so the courier binary stays CGO-free.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/agentkourier/agent-kourier/internal/store"
)

// readConns is the size of the read-only connection pool.
const readConns = 4

// Store is a SQLite-backed store.Store.
//
// SQLite allows one writer at a time, so all writes go through a pool capped
// at a single connection: writers queue in Go instead of contending for the
// file lock and failing with "database is locked". Reads use a separate
// read-only pool, which WAL mode lets run alongside the writer.
type Store struct {
	write *sql.DB
	read  *sql.DB
}

// Open opens (creating if needed) the database at path in WAL mode with
// foreign keys on and a busy timeout, and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	s := &Store{}
	var err error
	if s.write, err = openPool(ctx, path, 1, "journal_mode(WAL)"); err != nil {
		return nil, fmt.Errorf("open sqlite store %q (write pool): %w", path, err)
	}
	if s.read, err = openPool(ctx, path, readConns, "query_only(1)"); err != nil {
		_ = s.write.Close()
		return nil, fmt.Errorf("open sqlite store %q (read pool): %w", path, err)
	}
	if err = migrate(ctx, s.write, migrationFiles); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return s, nil
}

func openPool(ctx context.Context, path string, conns int, pragma string) (*sql.DB, error) {
	q := url.Values{"_pragma": {"busy_timeout(5000)", "foreign_keys(1)", pragma}}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(conns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return db, nil
}

// Close closes both connection pools.
func (s *Store) Close() error {
	return errors.Join(s.write.Close(), s.read.Close())
}

func ts(t time.Time) int64     { return t.UnixNano() }
func fromTS(n int64) time.Time { return time.Unix(0, n).UTC() }

// execer is implemented by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execRows runs a write on e and reports the number of rows it changed. Pass a
// *sql.Tx to run several statements atomically (see inTx).
func execRows(ctx context.Context, e execer, query string, args ...any) (int64, error) {
	res, err := e.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// exec runs a single-statement write on the write pool.
func (s *Store) exec(ctx context.Context, query string, args ...any) (int64, error) {
	return execRows(ctx, s.write, query, args...)
}

// inTx runs fn in a transaction on the write pool, committing if fn returns
// nil and rolling back on an error or panic. The pool has one connection, so fn must use tx and never s.write or
// s.exec, which would block until the context ends.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit; also covers a panic in fn
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// errIfUnchanged turns "no rows changed" into ErrNotFound.
func errIfUnchanged(n int64, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, store.ErrNotFound)
	}
	return nil
}

// AppendEvent implements store.Events.
func (s *Store) AppendEvent(ctx context.Context, e store.Event) (bool, error) {
	if e.ID == "" || e.ReceivedAt.IsZero() {
		return false, fmt.Errorf("append event %q: ID and ReceivedAt are required: %w", e.ID, store.ErrInvalid)
	}
	if e.Payload == nil {
		e.Payload = []byte{} // a nil slice would bind as NULL
	}
	n, err := s.exec(ctx, `INSERT INTO events (id, kind, binding_key, payload, received_at, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		e.ID, e.Kind, e.BindingKey, e.Payload, ts(e.ReceivedAt), ts(e.ReceivedAt))
	if err != nil {
		return false, fmt.Errorf("append event %q: %w", e.ID, err)
	}
	return n > 0, nil
}

// GetEvent implements store.Events.
func (s *Store) GetEvent(ctx context.Context, id string) (store.Event, error) {
	var e store.Event
	var received, next int64
	err := s.read.QueryRowContext(ctx, `SELECT id, kind, binding_key, payload, received_at, status, attempts,
		next_attempt_at, last_error FROM events WHERE id = ?`, id).
		Scan(&e.ID, &e.Kind, &e.BindingKey, &e.Payload, &received, &e.Status, &e.Attempts, &next, &e.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Event{}, fmt.Errorf("get event %q: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return store.Event{}, fmt.Errorf("get event %q: %w", id, err)
	}
	e.ReceivedAt, e.NextAttemptAt = fromTS(received), fromTS(next)
	return e, nil
}

// SetEventStatus implements store.Events.
func (s *Store) SetEventStatus(ctx context.Context, id string, status store.EventStatus, lastError string) error {
	if !status.Valid() {
		return fmt.Errorf("set event %q status %q: %w", id, status, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `UPDATE events SET status = ?, last_error = ? WHERE id = ?`, string(status), lastError, id)
	return errIfUnchanged(n, err, fmt.Sprintf("set event %q status", id))
}
