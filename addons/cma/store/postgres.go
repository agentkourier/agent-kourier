package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// migrateLockKey is the advisory lock each migration transaction takes, so that replicas that start together apply
// a migration one at a time. It is the adapter's own, apart from any key the broker uses in a database they share.
const migrateLockKey int64 = 0x636d615f6d696772 // "cma_migr"

// OpenPostgres connects to the Postgres database at dsn and applies its pending migrations. Several replicas may open
// the same database. An error never shows dsn, which may hold a password.
func OpenPostgres(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("open postgres store: POSTGRES_DSN is not a connection string pgx can read (its value is not shown)")
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
	sub, err := fs.Sub(postgresMigrations, "migrations/postgres")
	if err == nil {
		err = migrate(ctx, c, sub, s.log)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
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
