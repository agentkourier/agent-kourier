package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
}

// listMigrations parses the migrations/<version>_<name>.sql files in fsys and
// returns them sorted by numeric version. Two files with one version are an
// error, so a clash fails loudly instead of one file being skipped.
func listMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	seen := map[int]string{}
	var ms []migration
	for _, name := range files {
		v, err := strconv.Atoi(strings.SplitN(path.Base(name), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q: filename must start with a numeric version: %w", name, err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, name, v)
		}
		seen[v] = name
		ms = append(ms, migration{v, name})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return ms, nil
}

// migrate applies the migrations in fsys that are not yet recorded in
// schema_migrations, in version order. Each runs in its own transaction
// together with its schema_migrations row, so a failure leaves earlier
// migrations applied and the failing one not at all.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := listMigrations(fsys)
	if err != nil {
		return err
	}
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range ms {
		if err := applyMigration(ctx, db, fsys, m); err != nil {
			return fmt.Errorf("migration %q: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, time.Now().UnixNano())
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
	body, err := fs.ReadFile(fsys, m.name)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return tx.Commit()
}
