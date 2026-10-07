package postgres

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestTheStoreKeepsItsOptionalInterfaces: callers ask the store they hold for more than store.Store (the broker for
// Fenced, which is what makes it wait for a lease, and for metrics; the interactions package for the claim of a reply's
// question), and the guarded store that Open returns answers each as the adapter always did. No server is needed: the
// answers are in the type, and an empty holder is refused before the pool is touched.
func TestTheStoreKeepsItsOptionalInterfaces(t *testing.T) {
	var s store.Store = guard(&adapter{})
	fenced, ok := s.(store.Fenced)
	if !ok {
		t.Fatal("the Postgres store is not Fenced: the broker would run every replica as leader")
	}
	if _, err := fenced.Lead(t.Context(), ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Lead with no holder = %v; want store.ErrInvalid", err)
	}
	if _, ok := s.(interface {
		RegisterMetrics(prometheus.Registerer) error
	}); !ok {
		t.Error("the store has no RegisterMetrics")
	}
	if _, ok := s.(store.ReplyInteractionClaimer); !ok {
		t.Error("the store has no ClaimReplyInteraction: text replies need it")
	}
}
