package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the SQLite driver

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

// noun is what the kit's messages call the agent's process.
const noun = "agent"

// OpenSQLite opens (creating it, and its directory, if needed) the SQLite store at path and applies its pending
// migrations, the kit's and the launcher's. A second agent on the same file fails with kitstore.ErrInUse instead
// of sharing it (kitstore.OpenSQLite). Writes, go through the kit's one write connection. A file
// the agent wrote before it kept its tasks in this layout has them imported (legacy.go).
func OpenSQLite(ctx context.Context, path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open sqlite store %q: create its directory: %w", path, err)
	}
	var legacy bool
	prepare := func(ctx context.Context, q kitstore.Querier) (err error) {
		legacy, err = setLegacyAside(ctx, q)
		return err
	}
	db, err := kitstore.OpenSQLite(ctx, path, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed, Prepare: prepare})
	if err != nil {
		return nil, err
	}
	s := newStore(db, opts)
	if err := s.openSQLite(ctx, legacy); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return s, nil
}

func (s *Store) openSQLite(ctx context.Context, legacy bool) error {
	sub, err := fs.Sub(sqliteMigrations, "migrations/sqlite")
	if err != nil {
		return err
	}
	if err := s.Migrate(ctx, sub); err != nil {
		return err
	}
	if legacy {
		if err := s.importLegacy(ctx); err != nil {
			return err
		}
	}
	return nil
}
