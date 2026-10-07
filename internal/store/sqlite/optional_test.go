package sqlite

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestTheStoreKeepsItsOptionalInterfaces: callers ask the store they hold for more than store.Store (the broker for
// Fenced and for metrics, the interactions package for the claim of a reply's question), and the guarded store that Open
// returns answers each as the adapter always did. SQLite is not Fenced: one replica opens its file.
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
	claims, ok := s.(store.ReplyInteractionClaimer)
	if !ok {
		t.Fatal("the store has no ClaimReplyInteraction: text replies need it")
	}
	ctx := t.Context()
	if _, err := s.EnqueueReply(ctx, store.Reply{ID: "r1", Key: key, UserID: "U1", MessageRef: "1.2", EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := claims.ClaimReplyInteraction(ctx, "r1", "q1"); err != nil {
		t.Errorf("ClaimReplyInteraction = %v", err)
	}
	if err := claims.ClaimReplyInteraction(ctx, "r1", ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("ClaimReplyInteraction with no question = %v; want store.ErrInvalid", err)
	}
}
