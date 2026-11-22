package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// migrateLockKey is the advisory lock each migration transaction takes, so that replicas that start together apply
// a migration one at a time. It is the adapter's own, apart from any key the broker uses in a database they share.
const migrateLockKey int64 = 0x636d615f6d696772 // "cma_migr"

// The server-side timeouts each connection starts with, unless the DSN sets them: a transaction left idle (its client
// stalled or cut off) is ended, which frees the leadership row it may hold, and no statement runs for longer than
// the store's slowest should. They are the broker's defaults.
const (
	idleInTxTimeout  = 10 * time.Second
	statementTimeout = 30 * time.Second
)

// OpenPostgres connects to the Postgres database at dsn and applies its pending migrations. Several replicas may open
// the same database, and they elect a leader through it (TryLead): every write checks, in its transaction, that this
// store holds the current lease, and fails with ErrDeposed otherwise, so a store that has not taken the lease writes
// nothing. An error never shows dsn, which may hold a password.
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
	c := &pgConn{pool: pool}
	s := newStore(c, opts)
	s.elects = true
	sub, err := fs.Sub(postgresMigrations, "migrations/postgres")
	if err == nil {
		err = migrate(ctx, c, sub, s.log)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
	s.fence = s.checkEpoch // after the migrations, which write without a lease
	return s, nil
}

// pgConn is the Postgres backend's pool.
type pgConn struct{ pool *pgxpool.Pool }

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

func (c *pgConn) close() error {
	c.pool.Close()
	return nil
}
