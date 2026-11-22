package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
)

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// migrateLockKey is the advisory lock each migration transaction, and ADK's migration, takes, so that replicas that
// start together migrate one at a time. It is the agent's own, apart from any key the broker or another add-on uses
// in a database they share.
const migrateLockKey int64 = 0x61785f6d69677261 // "ax_migra"

// The server-side timeouts each connection starts with, unless the DSN sets them: a transaction left idle (its client
// stalled or cut off) is ended, and no statement runs for longer than the store's slowest should. They are the
// broker's defaults.
const (
	idleInTxTimeout  = 10 * time.Second
	statementTimeout = 30 * time.Second
)

// OpenPostgres connects to the Postgres database at dsn and applies its pending migrations and ADK's. Several replicas
// may open the same database, and elect a leader through it (lease.go): every write, the store's and ADK's, fails
// with ErrDeposed unless this store holds the lease. ADK's sessions go through gorm on the same pool. An error never shows dsn, which may
// hold a password.
func OpenPostgres(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("open postgres store: POSTGRES_DSN is not a connection string pgx can read (its value is not shown)")
	}
	for name, d := range map[string]time.Duration{"idle_in_transaction_session_timeout": idleInTxTimeout, "statement_timeout": statementTimeout} {
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
	c := &pgConn{pool: pool, std: stdlib.OpenDBFromPool(pool)}
	s := newStore(c, opts)
	s.elects = true
	sub, err := fs.Sub(postgresMigrations, "migrations/postgres")
	if err == nil {
		err = migrate(ctx, c, sub, s.log)
	}
	if err == nil {
		err = s.openADK(ctx, postgres.New(postgres.Config{Conn: c.std}), c.lockADKMigration)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
	s.fence = s.checkEpoch // after the migrations, the store's and ADK's, which write without a lease
	return s, nil
}

// pgConn is the Postgres backend's pool, and the database/sql view of it that gorm uses.
type pgConn struct {
	pool *pgxpool.Pool
	std  *sql.DB
}

func (c *pgConn) reader() querier { return pgQuerier{db: c.pool} }

func (c *pgConn) inTx(ctx context.Context, fn func(q querier) error) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error { return fn(pgQuerier{db: tx}) })
}

func (c *pgConn) lockMigrations(ctx context.Context, q querier) error {
	var locked string
	if err := q.queryRow(ctx, `SELECT pg_advisory_xact_lock(?)::text`, migrateLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	return nil
}

// lockADKMigration runs fn, ADK's gorm migration, while a connection taken out of the pool holds the migration lock:
// gorm migrates on connections of its own, outside any transaction of the store's. Closing that connection ends its
// session, and the lock with it, however fn ends.
func (c *pgConn) lockADKMigration(ctx context.Context, fn func() error) error {
	held, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	conn := held.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	return fn()
}

func (c *pgConn) over(db sqlDB) querier { return sqlQuerier{db: db, numbered: true} }

func (c *pgConn) close() error {
	err := c.std.Close()
	c.pool.Close()
	return err
}
