package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/store/storetest"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

func TestSQLiteConformance(t *testing.T) {
	storetest.Run(t, SQLiteHarness())
}

func TestPostgresConformance(t *testing.T) {
	storetest.Run(t, PostgresHarness(t))
}

// SQLiteHarness opens SQLite stores in temporary directories.
func SQLiteHarness() storetest.Harness {
	return storetest.Harness{
		New: func(t *testing.T) string { return filepath.Join(t.TempDir(), "cma.db") },
		Open: func(t *testing.T, path string, now func() time.Time) *store.Store {
			t.Helper()
			s, err := store.OpenSQLite(context.Background(), path, store.Options{Now: now})
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
}

// PostgresHarness opens Postgres stores, each location a schema of its own that the test drops after, and has each
// take the lease. It skips the test when pgtest.DSNEnv is unset.
func PostgresHarness(t *testing.T) storetest.Harness {
	t.Helper()
	pgtest.DSN(t) // skips when there is no Postgres
	return storetest.Harness{
		New: func(t *testing.T) string { return pgtest.DSN(t) },
		Open: func(t *testing.T, location string, now func() time.Time) *store.Store {
			t.Helper()
			s, err := store.OpenPostgres(context.Background(), location, store.Options{Now: now})
			if err != nil {
				t.Fatalf("OpenPostgres: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			// A Postgres store writes only while it holds the lease. One holder for every open: a reopen takes it over.
			if _, ok, err := s.TryLead(context.Background(), "storetest", "http://storetest.invalid:8090", time.Hour); err != nil || !ok {
				t.Fatalf("TryLead: %v, %v", ok, err)
			}
			return s
		},
	}
}
