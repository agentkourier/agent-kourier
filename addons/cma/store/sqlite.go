package store

import (
	"context"

	_ "modernc.org/sqlite" // registers the "sqlite" driver the kit opens: pure Go, so the adapter stays CGO-free

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// noun is what the kit's messages call the adapter's process.
const noun = "adapter"

// OpenSQLite opens (creating if needed) the SQLite store at path and applies the kit's pending migrations. A second
// adapter on the same file fails with kitstore.ErrInUse instead of sharing it (kitstore.OpenSQLite). A file an earlier
// build of the adapter wrote fails with ErrEarlierLayout, unchanged.
func OpenSQLite(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := kitstore.OpenSQLite(ctx, path, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed,
		Prepare: refuseEarlierLayout(`SELECT COUNT(*) > 0 FROM sqlite_master WHERE type = 'table' AND name IN ('context_sessions', 'schema_migrations')`)})
	if err != nil {
		return nil, err
	}
	return newStore(db, opts), nil
}
