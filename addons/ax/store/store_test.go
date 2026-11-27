package store_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
	kitstoretest "github.com/agentkourier/agent-kourier/addons/kit/store/storetest"
)

// The kit's task state, on the database the agent opens: its driver, its legacy import, and its launch migrations.
func TestSQLiteTaskStateConformance(t *testing.T) {
	kitstoretest.Run(t, kitstoretest.Harness{
		New: sqliteLocation,
		Open: func(t *testing.T, path string, log *slog.Logger) *kitstore.DB {
			return openSQLite(t, path, store.Options{Log: log}).DB
		},
	})
}

func TestPostgresTaskStateConformance(t *testing.T) {
	pgtest.DSN(t) // skips when there is no Postgres
	kitstoretest.Run(t, kitstoretest.Harness{
		New: pgLocation,
		Open: func(t *testing.T, location string, log *slog.Logger) *kitstore.DB {
			return openPostgres(t, location, store.Options{Log: log}).DB
		},
	})
}

func sqliteLocation(t *testing.T) string { return filepath.Join(t.TempDir(), "ax.db") }

// pgLocation is a schema of its own, which the test drops after.
func pgLocation(t *testing.T) string { return pgtest.DSN(t) }

func openSQLite(t *testing.T, path string, opts store.Options) *store.Store {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openPostgres opens the store at location and takes the lease: a Postgres store writes only while it holds it. One
// holder for every open: a reopen takes it over.
func openPostgres(t *testing.T, location string, opts store.Options) *store.Store {
	t.Helper()
	s, err := store.OpenPostgres(context.Background(), location, opts)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, ok, err := s.TryLead(context.Background(), "storetest", "http://storetest.invalid:8090", time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
	return s
}
