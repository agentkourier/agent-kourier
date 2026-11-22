package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	_ "modernc.org/sqlite" // registers the "sqlite" driver the kit opens: pure Go, so the adapter stays CGO-free

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

// noun is what the kit's messages call the adapter's process.
const noun = "adapter"

// OpenSQLite opens (creating if needed) the SQLite store at path and applies its pending migrations, the kit's and
// then the adapter's. A second adapter on the same file fails with kitstore.ErrInUse instead of sharing it
// (kitstore.OpenSQLite).
func OpenSQLite(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := kitstore.OpenSQLite(ctx, path, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed})
	if err != nil {
		return nil, err
	}
	s := newStore(db, opts)
	sub, err := fs.Sub(sqliteMigrations, "migrations/sqlite")
	if err == nil {
		err = s.Migrate(ctx, sub)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	return s, nil
}
