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

// migrateLockKey is the advisory lock each migration transaction takes, so that replicas that start together apply
// a migration one at a time. It is the adapter's own, apart from any key the broker uses in a database they share.
const migrateLockKey int64 = 0x636d615f6d696772 // "cma_migr"

// OpenPostgres connects to the Postgres database at dsn and applies its pending migrations, the kit's and then the
// adapter's. Several replicas may open the same database, and they elect a leader through it (TryLead): every write
// checks, in its transaction, that this store holds the current lease, and fails with kitstore.ErrDeposed otherwise,
// so a store that has not taken the lease writes nothing. An error never shows dsn, which may hold a password.
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
