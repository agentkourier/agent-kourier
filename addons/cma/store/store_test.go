package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/store/storetest"
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

// PostgresHarness opens Postgres stores, each location a schema of its own that the test drops after. It skips the
// test when store.PostgresDSNEnv is unset.
func PostgresHarness(t *testing.T) storetest.Harness {
	t.Helper()
	store.PostgresTestDSN(t) // skips when there is no Postgres
	return storetest.Harness{
		New: store.PostgresTestDSN,
		Open: func(t *testing.T, location string, now func() time.Time) *store.Store {
			t.Helper()
			s, err := store.OpenPostgres(context.Background(), location, store.Options{Now: now})
			if err != nil {
				t.Fatalf("OpenPostgres: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
}
