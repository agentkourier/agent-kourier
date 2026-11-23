package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// The server-side timeouts each connection starts with, unless the DSN sets them: a transaction left idle (its client
// stalled or cut off) is ended, which frees the leadership row it may hold, and no statement runs for longer than the
// store's slowest should. They are the broker's defaults.
const (
	// PostgresIdleInTxTimeout is the connections' idle_in_transaction_session_timeout.
	PostgresIdleInTxTimeout = 10 * time.Second
	// PostgresStatementTimeout is the connections' statement_timeout.
	PostgresStatementTimeout = 30 * time.Second
)

// OpenPostgres connects to the Postgres database at dsn and applies the kit's pending migrations; Migrate applies the
// add-on's. Several replicas may open the same database, and they elect a leader through it (TryLead): every write
// checks, in its transaction, that this database holds the current lease, and fails with ErrDeposed otherwise, so a
// database that has not taken the lease writes nothing. Migrations write without the lease. An error never shows dsn,
// which may hold a password.
func OpenPostgres(ctx context.Context, dsn string, opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("open postgres store: POSTGRES_DSN is not a connection string pgx can read (its value is not shown)")
	}
	for name, d := range map[string]time.Duration{"idle_in_transaction_session_timeout": PostgresIdleInTxTimeout, "statement_timeout": PostgresStatementTimeout} {
		if _, set := cfg.ConnConfig.RuntimeParams[name]; !set {
			cfg.ConnConfig.RuntimeParams[name] = strconv.FormatInt(d.Milliseconds(), 10)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("open postgres store: connect: %w", err)
	}
	c := &pgConn{pool: pool, std: stdlib.OpenDBFromPool(pool), lockKey: opts.MigrationLockKey}
	db := newDB(c, opts)
	db.elects = true
	if err := db.migrateKit(ctx, "migrations/postgres"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
	db.setFence(db.checkEpoch) // after the kit's migrations; every migration writes without a lease
	return db, nil
}

// pgConn is the Postgres backend's pool, and the database/sql view of it that SQL returns.
type pgConn struct {
	pool    *pgxpool.Pool
	std     *sql.DB
	lockKey int64
}

func (c *pgConn) reader() Querier { return pgQuerier{db: c.pool} }

func (c *pgConn) inTx(ctx context.Context, fn func(q Querier) error) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error { return fn(pgQuerier{db: tx}) })
}

func (c *pgConn) lockMigrations(ctx context.Context, q Querier) error {
	var locked string
	if err := q.QueryRow(ctx, `SELECT pg_advisory_xact_lock(?)::text`, c.lockKey).Scan(&locked); err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	return nil
}

// withMigrationLock runs fn while a connection taken out of the pool holds the migration lock: fn migrates on
// connections of its own, outside any transaction of the store's. Closing that connection ends its session, and the
// lock with it, however fn ends.
func (c *pgConn) withMigrationLock(ctx context.Context, fn func() error) error {
	held, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	conn := held.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, c.lockKey); err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	return fn()
}

func (c *pgConn) over(db SQLDB) Querier { return sqlQuerier{db: db, numbered: true} }

func (c *pgConn) sql() *sql.DB { return c.std }

func (c *pgConn) close() error {
	err := c.std.Close()
	c.pool.Close()
	return err
}
