package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate applies the migrations in fsys that are not yet recorded in
// schema_migrations, in version order. Each runs in its own transaction
// together with its schema_migrations row, so a failure leaves earlier
// migrations applied and the failing one not at all. It then reports, at
// WARN, a database that records a migration fsys does not have
// (store.WarnIfSchemaNewer).
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS, log *slog.Logger) error {
	ms, err := store.ListMigrations(fsys)
	if err != nil {
		return err
	}
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range ms {
		if err := applyMigration(ctx, db, fsys, m); err != nil {
			return fmt.Errorf("migration %q: %w", m.Name, err)
		}
	}
	return warnIfNewer(ctx, db, ms, log)
}

// warnIfNewer reads the newest version schema_migrations records and hands it to store.WarnIfSchemaNewer.
func warnIfNewer(ctx context.Context, db *sql.DB, known []store.Migration, log *slog.Logger) error {
	var newest int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&newest); err != nil {
		return fmt.Errorf("read the newest migration: %w", err)
	}
	store.WarnIfSchemaNewer(ctx, log, known, newest)
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, m store.Migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.Version, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if n == 0 {
		return tx.Rollback() // already applied
	}
	body, err := fs.ReadFile(fsys, m.Name)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return tx.Commit()
}
