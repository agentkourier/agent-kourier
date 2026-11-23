package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	// The gorm dialector ADK's sessions go through. It registers the "sqlite" database/sql driver
	// (github.com/glebarez/go-sqlite, a pure Go build of SQLite, so the agent stays CGO-free), which the kit's store
	// opens too: one driver for both, since modernc.org/sqlite registers the same name and cannot be linked beside it.
	"github.com/glebarez/sqlite"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

// noun is what the kit's messages call the agent's process.
const noun = "agent"

// OpenSQLite opens (creating it, and its directory, if needed) the SQLite store at path and applies its pending
// migrations, the kit's, the agent's and ADK's. A second agent on the same file fails with kitstore.ErrInUse instead
// of sharing it (kitstore.OpenSQLite). Writes, the store's and ADK's, go through the kit's one write connection. A file
// the agent wrote before it kept its tasks in this layout has them imported (legacy.go).
func OpenSQLite(ctx context.Context, path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open sqlite store %q: create its directory: %w", path, err)
	}
	db, err := kitstore.OpenSQLite(ctx, path, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed})
	if err != nil {
		return nil, err
	}
	s := newStore(db, opts)
	if err := s.openSQLite(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return s, nil
}

func (s *Store) openSQLite(ctx context.Context) error {
	legacy, err := setLegacyAside(ctx, s.DB)
	if err != nil {
		return err
	}
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
	return s.openADK(ctx, sqlite.Dialector{Conn: s.SQL()})
}
