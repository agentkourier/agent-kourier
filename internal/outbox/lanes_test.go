package outbox_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestForgetDropsTheLaneOfAConnection: a connection that delivered keeps a lane, and Forget takes it away, so that the lanes do
// not grow with the connections a broker has served. The lane of another connection stays, and a post of the forgotten connection
// makes its lane again.
func TestForgetDropsTheLaneOfAConnection(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	apps := newTwoApps()
	o := mustNew(t, outbox.Config{Store: st, Platforms: apps.set, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: quiet()})
	ctx := t.Context()
	for _, c := range []string{connA, connB} {
		if _, err := o.Send(ctx, onConn(c, "sent-"+c)); err != nil {
			t.Fatalf("Send on %s: %v", c, err)
		}
	}
	if got := o.Lanes(); got != 2 {
		t.Fatalf("Lanes = %d after a post on each of two connections; want 2", got)
	}

	o.Forget(connB)
	o.Forget("ns/never-posted") // no lane: nothing to do

	if got := o.Lanes(); got != 1 {
		t.Errorf("Lanes = %d after Forget of b; want a's lane alone", got)
	}
	if _, err := o.Send(ctx, onConn(connB, "again")); err != nil {
		t.Fatalf("Send on b after Forget: %v", err)
	}
	if got := o.Lanes(); got != 2 {
		t.Errorf("Lanes = %d after b posted again; want its lane made anew", got)
	}
}

// TestForgetWaitsForADeliveryInFlight: a lane that a post is being made on is not taken from under it, which would let a second
// delivery of the connection begin beside it; it goes when that post is done.
func TestForgetWaitsForADeliveryInFlight(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	release := make(chan struct{})
	a := hangs(release)
	o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connA: a},
		Now: func() time.Time { return t0 }, CallTimeout: time.Minute, Logger: quiet()})
	sent := make(chan error, 1)
	go func() { _, err := o.Send(t.Context(), onConn(connA, "stuck")); sent <- err }()
	eventually(t, "the post to be in flight", func() bool { posts, _ := a.counts(); return posts == 1 })

	o.Forget(connA)
	if got := o.Lanes(); got != 1 {
		t.Errorf("Lanes = %d with a post in flight after Forget; want the lane kept until it is done", got)
	}

	close(release)
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := o.Lanes(); got != 0 {
		t.Errorf("Lanes = %d once the post is done; want the forgotten lane gone", got)
	}
}

// TestAConnectionThatIsDownLeavesNoLane: the rows of a connection nothing runs wait for it, in the drain and in Send, and neither
// keeps a lane for it: a removed connection's rows can wait up to the retention, and a lane for each would outlive them.
func TestAConnectionThatIsDownLeavesNoLane(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	res := &dynamic{m: map[string]outbox.Platform{connA: &poster{}}}
	o := mustNew(t, outbox.Config{Store: st, Platforms: res, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: quiet()})
	ctx := t.Context()
	waiting := onConn(connB, "waiting-b")
	waiting.Role = store.OutboxNotice
	if _, err := o.Enqueue(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Send(ctx, onConn(connA, "going-a")); err != nil {
		t.Fatal(err)
	}

	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with b down: %v", err)
	}
	if _, err := o.Send(ctx, onConn(connB, "sent-b")); err == nil {
		t.Fatal("Send on a connection that is down succeeded")
	}

	if got := o.Lanes(); got != 1 {
		t.Errorf("Lanes = %d with b down; want a's lane alone", got)
	}
}
