package sqlite

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestTheStoreKeepsItsOptionalInterfaces: the broker asks the store it holds for more than store.Store (Fenced, and
// metrics), and the guarded store that Open returns answers each as the adapter always did. SQLite is not Fenced: one
// replica opens its file.
func TestTheStoreKeepsItsOptionalInterfaces(t *testing.T) {
	var s store.Store = openTemp(t)
	if _, ok := s.(interface {
		RegisterMetrics(prometheus.Registerer) error
	}); !ok {
		t.Error("the store has no RegisterMetrics")
	}
	if _, ok := s.(store.Fenced); ok {
		t.Error("the SQLite store is Fenced: it would make the broker wait for a lease that nothing grants")
	}
}
