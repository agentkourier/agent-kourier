package sqlite

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/storetest"
)

// TestConformance holds the SQLite store to the suite that every store.Store implementation passes.
// The tests of this package that follow are about SQLite itself: its migrations, pragmas and SQL.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Instance {
		path := seededPath(t)
		current := openAt(t, path)
		return storetest.Instance{
			Store: current,
			Reopen: func(t *testing.T) store.Store {
				t.Helper()
				if err := current.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				current = openAt(t, path)
				return current
			},
		}
	})
}
