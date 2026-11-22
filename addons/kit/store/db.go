// Package store is the database under an add-on's state: SQLite (one file, one replica) or Postgres (shared by
// replicas, which elect a leader through it), with the kit's migrations and then the add-on's applied, and a fence that
// every write runs first. On Postgres the fence is the leader's lease epoch (lease.go): a replica that has not taken
// the lease, or has lost it, writes nothing. The add-on keeps its own tables on it, written through Write, and runs
// Fence in the transactions of a library that writes them on connections of its own (ADK's sessions, through gorm).
//
// Both backends run the same SQL, written with ? placeholders, which the Postgres querier numbers. The kit imports no
// SQLite driver: the add-on links one that registers "sqlite" (modernc.org/sqlite, or github.com/glebarez/go-sqlite,
// which registers the same name, so the two cannot be linked together).
package store

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a database.
type Options struct {
	// Noun is what the store's messages call the add-on's process: "adapter" makes "another adapter has the SQLite file
	// open". Empty is "add-on".
	Noun string
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write finds that another replica has taken the lease since this database took
	// it, before the write fails with ErrDeposed. It runs in the writer's goroutine and must not call the database. The
	// add-on ends its process in it: the error alone would reach whichever caller the write was made for. It does not
	// run for a database that holds no epoch (a standby's, or one that released its lease).
	OnDeposed func()
	// MigrationLockKey, on Postgres, is the advisory lock each migration transaction takes, the kit's and the add-on's,
	// so that replicas that start together migrate one at a time. It is the add-on's own, apart from any key the broker
	// or another add-on uses in a database they share.
	MigrationLockKey int64
}

// DB is an add-on's database on SQLite or Postgres, with the kit's migrations applied.
type DB struct {
	conn conn
	noun string
	log  *slog.Logger

	// fence, when set, runs first in every write transaction, Write's and the add-on's own (Fence), and its error aborts
	// the write. It is the one place a check that must hold for any write runs: a leader's lease epoch, on Postgres,
	// where several replicas share the database.
	fence func(ctx context.Context, q Querier) error
	// elects is true on Postgres, where replicas elect a leader through the leadership row (lease.go).
	elects bool
	// epoch is the lease epoch this database took, 0 for none.
	epoch     atomic.Int64
	onDeposed func()

	closeOnce sync.Once
	closeErr  error
}

func newDB(c conn, opts Options) *DB {
	db := &DB{conn: c, noun: opts.Noun, log: opts.Log, onDeposed: opts.OnDeposed}
	if db.noun == "" {
		db.noun = "add-on"
	}
	if db.log == nil {
		db.log = slog.Default()
	}
	return db
}

// kitMigrations is the version table of the kit's own migrations, which come before the add-on's.
const kitMigrations = "kit_schema_migrations"

// addonMigrations is the version table of the add-on's migrations.
const addonMigrations = "schema_migrations"

// migrateKit applies the kit's migrations of the backend whose files are in dir.
func (db *DB) migrateKit(ctx context.Context, dir string) error {
	sub, err := fs.Sub(kitMigrationFiles, dir)
	if err != nil {
		return err
	}
	return migrate(ctx, db.conn, kitMigrations, sub, db.noun, db.log)
}

// Migrate applies the add-on's migrations in fsys, files named NNNN_name.sql, that schema_migrations does not record,
// after the kit's, which the open applied. See migrate for how.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) error {
	return migrate(ctx, db.conn, addonMigrations, fsys, db.noun, db.log)
}

// Reader runs reads outside a transaction.
func (db *DB) Reader() Querier { return db.conn.reader() }

// Write runs fn in one write transaction, after the fence; the fence's error, or fn's, rolls it back. Every write of
// the add-on's tables goes through it, or runs Fence in its own transaction.
func (db *DB) Write(ctx context.Context, fn func(q Querier) error) error {
	return db.conn.inTx(ctx, func(q Querier) error {
		if db.fence != nil {
			if err := db.fence(ctx, q); err != nil {
				return err
			}
		}
		return fn(q)
	})
}

// Fence runs the fence in tx, a transaction that a library began on SQL's connections (ADK's, through gorm), before
// the transaction's first write: its error must roll the transaction back, as it does a Write. It returns nil where
// there is no fence (SQLite).
func (db *DB) Fence(ctx context.Context, tx SQLDB) error {
	if db.fence == nil {
		return nil
	}
	return db.fence(ctx, db.conn.over(tx))
}

// SetFence replaces the fence that Write and Fence run: nil runs none. Only tests call it, to see that every write of
// an add-on runs the fence in its transaction; OpenPostgres sets the lease's epoch check, and it must not be changed
// while the database is in use.
func (db *DB) SetFence(fence func(ctx context.Context, q Querier) error) { db.fence = fence }

// SQL is the write connections as a database/sql DB, for a library that keeps its tables in the add-on's database
// (ADK's sessions, through gorm): on SQLite the one write connection, which its transactions share with Write's, on
// Postgres the pool. Its writes are fenced only where they run Fence.
func (db *DB) SQL() *sql.DB { return db.conn.sql() }

// WithMigrationLock runs fn, a migration of a library that migrates on connections of its own (ADK's, gorm's
// AutoMigrate), while the migration lock is held: on Postgres the advisory lock of MigrationLockKey, on a connection of
// its own, so that replicas that start together migrate one at a time. On SQLite the file lock already keeps every
// other replica out, and fn just runs.
func (db *DB) WithMigrationLock(ctx context.Context, fn func() error) error {
	return db.conn.withMigrationLock(ctx, fn)
}

// Close releases the database. Closing it again does nothing.
func (db *DB) Close() error {
	db.closeOnce.Do(func() { db.closeErr = db.conn.close() })
	return db.closeErr
}

func nanos(t time.Time) int64 { return t.UnixNano() }
