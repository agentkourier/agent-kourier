package outbox_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/outbox"
)

// TestThePurgeWaitsForAnInFlightDelivery: a row that is old by the time a drain starts, while its
// post is still in flight, is not purged under it. The drain waits, and the row ends posted, not
// abandoned, with no report.
func TestThePurgeWaitsForAnInFlightDelivery(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	var clock atomic.Int64
	inPost, release := make(chan struct{}), make(chan struct{})
	p := &poster{onPost: func(context.Context) error { close(inPost); <-release; return nil }}
	ob, hooks, _ := abandoning(t, st, p, func(c *outbox.Config) {
		c.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	})
	sent := make(chan error, 1)
	go func() { _, err := ob.Send(t.Context(), reply("ev1")); sent <- err }()
	<-inPost
	clock.Store(int64(8 * 24 * time.Hour)) // now past the default seven days
	reconciled := make(chan error, 1)
	go func() { reconciled <- ob.Reconcile(t.Context()) }()
	select {
	case err := <-reconciled:
		t.Fatalf("Reconcile finished while Send was posting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	row, _ := st.GetOutbox(t.Context(), "ev1/reply")
	if row.MessageRef == "" || !row.AbandonedAt.IsZero() || len(hooks.list()) != 0 {
		t.Fatalf("row = %+v, hooks %q; want it posted, not abandoned, and nothing reported", row, hooks.list())
	}
}
