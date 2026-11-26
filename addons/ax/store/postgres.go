package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// migrateLockKey is the advisory lock each migration transaction, and ADK's migration, takes, so that replicas that
// start together migrate one at a time. It is the agent's own, apart from any key the broker or another add-on uses
// in a database they share.
const migrateLockKey int64 = 0x61785f6d69677261 // "ax_migra"

// OpenPostgres applies migrations and opens a fenced AX store. The DSN
// may contain a password and is excluded from errors.
func OpenPostgres(ctx context.Context, dsn string, opts Options) (*Store, error) {
	db, err := kitstore.OpenPostgres(ctx, dsn, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed, MigrationLockKey: migrateLockKey})
	if err != nil {
		return nil, err
	}
	s := newStore(db, opts)
	sub, err := fs.Sub(postgresMigrations, "migrations/postgres")
	if err == nil {
		err = s.Migrate(ctx, sub)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open postgres store: %w", err)
	}
	return s, nil
}
