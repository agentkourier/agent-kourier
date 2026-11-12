package store

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Migration is one migrations/<version>_<name>.sql file of a backend.
type Migration struct {
	// Version is the number the file's name starts with, the key of its row in schema_migrations.
	Version int
	// Name is the file's path in the backend's embedded file system.
	Name string
}

// ListMigrations parses the migrations/<version>_<name>.sql files in fsys and returns them sorted by numeric
// version. Two files with one version are an error, so a clash fails loudly instead of one file being skipped.
func ListMigrations(fsys fs.FS) ([]Migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	seen := map[int]string{}
	var ms []Migration
	for _, name := range files {
		v, err := strconv.Atoi(strings.SplitN(path.Base(name), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q: filename must start with a numeric version: %w", name, err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, name, v)
		}
		seen[v] = name
		ms = append(ms, Migration{v, name})
	}
	slices.SortFunc(ms, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return ms, nil
}

// WarnIfSchemaNewer reports, at WARN, a schema that records a migration this binary does not have: a rollback to an
// older release, or a replica that has not been upgraded yet. known is what ListMigrations returned for the binary and
// newest is the highest version the database records (0 for none), which is the only thing a backend reads. It does
// not refuse; migration-specific compatibility notes govern whether an older binary can continue to use the schema.
// Both backends call it after they have migrated, so the message and its attributes are the same for both.
func WarnIfSchemaNewer(ctx context.Context, log *slog.Logger, known []Migration, newest int) {
	if n := len(known); n > 0 && newest > known[n-1].Version {
		log.WarnContext(ctx, "the database schema is newer than this build of Agent Kourier",
			"newest_known_migration", known[n-1].Version, "newest_in_database", newest)
	}
}
