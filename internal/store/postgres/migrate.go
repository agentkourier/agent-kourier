package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// migrationFiles holds the migrations. Their numbers and names are those of the SQLite store's, one for
// one: a migration that SQLite gets, Postgres gets under the same number, translated (a table is
// altered where SQLite needs a trigger, a time is a timestamptz where SQLite keeps an integer). A
// migration that only one store needs is an empty file in the other, saying why. TestMigrationsInStep
// fails when the two lists differ.
//
// The numbers are kept in step, and not folded into a baseline, so that a database's
// schema_migrations says the same thing whichever store made it, and a migration is reviewed once in
// two dialects, beside its twin.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate applies the migrations in fsys that schema_migrations does not record yet, in version order.
// Each runs in its own transaction with its schema_migrations row, so a failure leaves the earlier
// migrations applied and the failing one not at all (Postgres DDL is transactional).
//
// Every one of those transactions begins by taking an advisory lock, which is released when it ends.
// Replicas that start together therefore apply a migration one at a time: the first takes the lock and
// applies it, the others wait, and each finds it recorded when its turn comes. The lock is taken
// before the table of versions is created, as two sessions that run CREATE TABLE IF NOT EXISTS at once
// can fail on the catalog. It then reports, at WARN, a database that records a migration fsys does not have
// (store.WarnIfSchemaNewer).
func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	ms, err := store.ListMigrations(fsys)
	if err != nil {
		return err
	}
	for _, m := range ms {
		body, err := fs.ReadFile(fsys, m.Name)
		if err != nil {
			return fmt.Errorf("migration %q: read: %w", m.Name, err)
		}
		if err := applyMigration(ctx, pool, m, string(body)); err != nil {
			return fmt.Errorf("migration %q: %w", m.Name, err)
		}
	}
	return warnIfNewer(ctx, pool, ms, log)
}

// warnIfNewer reads the newest version schema_migrations records and hands it to store.WarnIfSchemaNewer.
func warnIfNewer(ctx context.Context, pool *pgxpool.Pool, known []store.Migration, log *slog.Logger) error {
	var newest int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&newest); err != nil {
		return fmt.Errorf("read the newest migration: %w", err)
	}
	store.WarnIfSchemaNewer(ctx, log, known, newest)
	return nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, m store.Migration, body string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// A migration may run long, and another replica may wait for it: neither is subject to the
		// statement timeout, for this transaction only.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			return fmt.Errorf("statement timeout: %w", err)
		}
		if err := lock(ctx, tx, lockMigrate, ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.Version).Scan(&applied); err != nil {
			return fmt.Errorf("check: %w", err)
		}
		if applied {
			return nil
		}
		if strings.TrimSpace(body) != "" {
			// No arguments, so the simple protocol runs the file's statements as the script they are.
			if _, err := tx.Exec(ctx, body); err != nil {
				return fmt.Errorf("exec: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		return nil
	})
}
