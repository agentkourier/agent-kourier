package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

// kitMigrationFiles are the kit's migrations, one directory per backend: the leadership row, which Postgres replicas
// elect a leader through. The add-on's own come after them, under a version table of their own.
//
//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var kitMigrationFiles embed.FS

// migration is one numbered SQL file: NNNN_name.sql.
type migration struct {
	version int
	name    string
}

// listMigrations returns the migrations in fsys in version order. A file not named NNNN_name.sql, or two with one
// number, is an error.
func listMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("migration %q is not named NNNN_name.sql", e.Name())
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, e.Name(), v)
		}
		seen[v] = e.Name()
		out = append(out, migration{version: v, name: e.Name()})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

// migrate applies the migrations in fsys that table, a version table, does not record, in version order, each in its
// own transaction with its row of table, so a failure leaves the earlier ones applied and the failing one not at all.
// Each transaction first takes the backend's migration lock, before it creates table. A database that records a
// migration fsys does not have (an older add-on after a newer one) is logged at WARN, not refused; noun names the
// add-on's process in that message and its key.
func migrate(ctx context.Context, c conn, table string, fsys fs.FS, noun string, log *slog.Logger) error {
	ms, err := listMigrations(fsys)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return errors.New("no migrations")
	}
	for _, m := range ms {
		body, err := fs.ReadFile(fsys, m.name)
		if err != nil {
			return fmt.Errorf("migration %q: read: %w", m.name, err)
		}
		err = c.inTx(ctx, func(q Querier) error {
			if err := c.lockMigrations(ctx, q); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (version integer PRIMARY KEY, applied_at bigint NOT NULL)`); err != nil {
				return fmt.Errorf("create %s: %w", table, err)
			}
			var applied int
			if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE version = ?`, m.version).Scan(&applied); err != nil {
				return fmt.Errorf("read %s: %w", table, err)
			}
			if applied > 0 {
				return nil
			}
			if _, err := q.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
			_, err := q.Exec(ctx, `INSERT INTO `+table+` (version, applied_at) VALUES (?, ?)`, m.version, nanos(time.Now()))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %q: %w", m.name, err)
		}
	}
	var newest int
	if err := c.reader().QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+table).Scan(&newest); err != nil {
		return fmt.Errorf("read the newest migration: %w", err)
	}
	if known := ms[len(ms)-1].version; newest > known {
		log.WarnContext(ctx, "the database records a migration this "+noun+" does not have; an older "+noun+" runs on a newer schema",
			"database_version", newest, noun+"_version", known)
	}
	return nil
}
