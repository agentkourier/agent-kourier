package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	// The gorm dialector ADK's sessions go through. It registers the "sqlite" database/sql driver
	// (github.com/glebarez/go-sqlite, a pure Go build of SQLite, so the agent stays CGO-free), which the store's own
	// connections use too: one driver for both, since modernc.org/sqlite registers the same name and cannot be linked
	// beside it.
	"github.com/glebarez/sqlite"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

// ErrInUse is returned by OpenSQLite when another agent has the file open. SQLite is one replica's store.
var ErrInUse = errors.New("another agent has the SQLite file open")

// sqliteReadConns is the size of the read pool.
const sqliteReadConns = 4

// OpenSQLite opens (creating it, and its directory, if needed) the SQLite store at path and applies its pending
// migrations and ADK's. It holds an exclusive lock on path + ".lock" until Close, so a second agent on the same file
// fails with ErrInUse instead of sharing it; the lock is the kernel's, so it goes with a process that dies. The file is
// in WAL mode with FULL synchronous commits and a busy timeout. Writes, the store's and ADK's, go through one
// connection, so writers queue in Go rather than contend for the file; the store's reads use a read-only pool beside
// it. A file the agent wrote before it kept its tasks in this layout has them imported (legacy.go).
func OpenSQLite(ctx context.Context, path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open sqlite store %q: create its directory: %w", path, err)
	}
	lock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	c := &sqliteConn{lock: lock}
	if c.write, err = sqlitePool(ctx, path, 1, "journal_mode(WAL)", "synchronous(FULL)"); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("open sqlite store %q (write pool): %w", path, err)
	}
	if c.read, err = sqlitePool(ctx, path, sqliteReadConns, "query_only(1)"); err != nil {
		_ = c.write.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("open sqlite store %q (read pool): %w", path, err)
	}
	s := newStore(c, opts)
	if err := s.openSQLite(ctx, c); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return s, nil
}

func (s *Store) openSQLite(ctx context.Context, c *sqliteConn) error {
	legacy, err := setLegacyAside(ctx, c)
	if err != nil {
		return err
	}
	sub, err := fs.Sub(sqliteMigrations, "migrations/sqlite")
	if err != nil {
		return err
	}
	if err := migrate(ctx, c, sub, s.log); err != nil {
		return err
	}
	if legacy {
		if err := s.importLegacy(ctx); err != nil {
			return err
		}
	}
	// The file lock keeps every other agent out, so ADK's migration needs no lock of its own.
	return s.openADK(ctx, sqlite.Dialector{Conn: c.write}, func(_ context.Context, fn func() error) error { return fn() })
}

func sqlitePool(ctx context.Context, path string, conns int, pragmas ...string) (*sql.DB, error) {
	q := url.Values{"_pragma": append([]string{"busy_timeout(5000)", "foreign_keys(1)"}, pragmas...)}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open(sqlite.DriverName, u.String())
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
	write *sql.DB // one connection, which gorm shares
	read  *sql.DB
	lock  *os.File
}

func (c *sqliteConn) reader() querier { return sqlQuerier{db: c.read} }

// inTx runs fn in a transaction on the write connection. fn must use the querier it is given: the write pool's one
// connection is the transaction's until it ends.
func (c *sqliteConn) inTx(ctx context.Context, fn func(q querier) error) error {
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

// lockMigrations does nothing: the file lock already keeps every other agent out.
func (c *sqliteConn) lockMigrations(context.Context, querier) error { return nil }

func (c *sqliteConn) over(db sqlDB) querier { return sqlQuerier{db: db} }

func (c *sqliteConn) close() error {
	return errors.Join(c.write.Close(), c.read.Close(), c.lock.Close())
}
