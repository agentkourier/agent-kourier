package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"
)

// ErrInUse is what OpenSQLite's error wraps when another process holds the file's lock. SQLite is one replica's store.
var ErrInUse = errors.New("another process has the SQLite file open")

// inUseError is ErrInUse in the add-on's words: "another adapter has the SQLite file open".
type inUseError struct{ noun string }

func (e inUseError) Error() string { return "another " + e.noun + " has the SQLite file open" }

// Is makes errors.Is(err, ErrInUse) hold.
func (e inUseError) Is(target error) bool { return target == ErrInUse }

// sqliteReadConns is the size of the read pool. A read past it waits in database/sql for a connection, for as long as
// its context lasts.
const sqliteReadConns = 4

// SQLiteBusyTimeout is how long a connection waits for a lock on the file before its statement fails with SQLITE_BUSY.
const SQLiteBusyTimeout = 5 * time.Second

// sqliteDriver is the database/sql driver the add-on links: modernc.org/sqlite and github.com/glebarez/go-sqlite both
// register this name.
const sqliteDriver = "sqlite"

// OpenSQLite opens (creating it if needed, in a directory that exists) the SQLite database at path, runs Prepare and
// applies the kit's pending migrations; Migrate applies the add-on's. It holds an exclusive lock on path + ".lock" until
// Close, so a second process on the same file fails with ErrInUse instead of sharing it; the lock is the kernel's, so it
// goes with a process that dies. The file is in WAL mode with FULL synchronous commits and a busy timeout. Writes,
// Write's and a library's on SQL, go through one connection, so writers queue in Go rather than contend for the file;
// Reader uses a read-only pool beside it. The add-on links the driver (sqliteDriver).
func OpenSQLite(ctx context.Context, path string, opts Options) (*DB, error) {
	c := &sqliteConn{}
	db := newDB(c, opts)
	lock, err := lockFile(path+".lock", db.noun)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	c.lock = lock
	if c.write, err = sqlitePool(ctx, path, 1, "journal_mode(WAL)", "synchronous(FULL)"); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("open sqlite store %q (write pool): %w", path, err)
	}
	if c.read, err = sqlitePool(ctx, path, sqliteReadConns, "query_only(1)"); err != nil {
		_ = c.write.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("open sqlite store %q (read pool): %w", path, err)
	}
	if err := db.migrateKit(ctx, "migrations/sqlite", opts.Prepare); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return db, nil
}

func sqlitePool(ctx context.Context, path string, conns int, pragmas ...string) (*sql.DB, error) {
	busy := fmt.Sprintf("busy_timeout(%d)", SQLiteBusyTimeout.Milliseconds())
	q := url.Values{"_pragma": append([]string{busy, "foreign_keys(1)"}, pragmas...)}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open(sqliteDriver, u.String())
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

// sqliteConn is the SQLite backend's connections and its lock.
type sqliteConn struct {
	write *sql.DB // one connection, which a library on SQL shares
	read  *sql.DB
	lock  *os.File
}

func (c *sqliteConn) reader() Querier { return sqlQuerier{db: c.read} }

// inTx runs fn in a transaction on the write connection. fn must use the Querier it is given: the write pool's one
// connection is the transaction's until it ends.
func (c *sqliteConn) inTx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := c.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := fn(sqlQuerier{db: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// lockMigrations does nothing: the file lock already keeps every other process out.
func (c *sqliteConn) lockMigrations(context.Context, Querier) error { return nil }

// withMigrationLock just runs fn: the file lock already keeps every other process out.
func (c *sqliteConn) withMigrationLock(_ context.Context, fn func() error) error { return fn() }

func (c *sqliteConn) over(db SQLDB) Querier { return sqlQuerier{db: db} }

func (c *sqliteConn) sql() *sql.DB { return c.write }

func (c *sqliteConn) close() error {
	return errors.Join(c.write.Close(), c.read.Close(), c.lock.Close())
}
