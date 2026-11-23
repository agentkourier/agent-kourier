package store

import (
	"context"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// migrateLockKey is the advisory lock each migration transaction takes, so that replicas that start together apply
// a migration one at a time. It is the adapter's own, apart from any key the broker uses in a database they share.
const migrateLockKey int64 = 0x636d615f6d696772 // "cma_migr"

// OpenPostgres connects to the Postgres database at dsn and applies the kit's pending migrations. Several replicas may
// open the same database, and they elect a leader through it (TryLead): every write checks, in its transaction, that
// this store holds the current lease, and fails with kitstore.ErrDeposed otherwise, so a store that has not taken the
// lease writes nothing. A database an earlier build of the adapter wrote fails with ErrEarlierLayout, unchanged. An
// error never shows dsn, which may hold a password.
func OpenPostgres(ctx context.Context, dsn string, opts Options) (*Store, error) {
	db, err := kitstore.OpenPostgres(ctx, dsn, kitstore.Options{Noun: noun, Log: opts.Log, OnDeposed: opts.OnDeposed, MigrationLockKey: migrateLockKey,
		Prepare: refuseEarlierLayout(`SELECT to_regclass('context_sessions') IS NOT NULL`)})
	if err != nil {
		return nil, err
	}
	return newStore(db, opts), nil
}
