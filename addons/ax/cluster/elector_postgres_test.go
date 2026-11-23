package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// replica is one agent process on a shared Postgres: its store and its elector.
type replica struct {
	store   *store.Store
	elector *Elector
	lease   *stallable
	done    <-chan error
	cancel  context.CancelFunc
}

// stallable is a store's lease whose renewals can be made to fail, as a leader cut off from the database's would.
type stallable struct {
	*store.Store
	stalled atomic.Bool
}

func (s *stallable) Renew(ctx context.Context, d time.Duration) error {
	if s.stalled.Load() {
		return errors.New("stalled")
	}
	return s.Store.Renew(ctx, d)
}

func startReplica(t *testing.T, dsn, name string, lease, deadline, retry time.Duration) *replica {
	t.Helper()
	st, err := store.OpenPostgres(context.Background(), dsn, store.Options{})
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &replica{store: st, lease: &stallable{Store: st}}
	r.elector = &Elector{Lease: r.lease, Holder: name, Address: "http://" + name + ":8090", LeaseDuration: lease,
		RenewDeadline: deadline, RetryPeriod: retry, Status: &Status{}, Log: discard()}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.done = run(ctx, r.elector, func(ctx context.Context) error { <-ctx.Done(); return nil })
	return r
}

func write(r *replica, id string) error {
	ctx, call := a2asrv.NewCallContext(context.Background(), nil)
	call.User = a2asrv.NewAuthenticatedUser("agent-kourier", nil)
	_, err := r.store.Create(ctx, &a2a.Task{ID: a2a.TaskID(id), ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	return err
}

// Two replicas on one database: one leads and writes, the other waits. When the leader can no longer renew, it stops at
// its renew deadline, the standby takes over once the lease has run out, with a new epoch, and the old leader's store
// can no longer write.
func TestPostgresElectionAndTakeover(t *testing.T) {
	dsn := pgtest.DSN(t)
	const lease, deadline, retry = 1500 * time.Millisecond, time.Second, 100 * time.Millisecond
	a := startReplica(t, dsn, "a", lease, deadline, retry)
	waitFor(t, "a to lead", a.elector.Status.Leading)
	b := startReplica(t, dsn, "b", lease, deadline, retry)
	time.Sleep(3 * retry)
	if b.elector.Status.Leading() {
		t.Fatal("two leaders")
	}
	if u, ok := b.elector.Leader(context.Background()); !ok || u.String() != "http://a:8090" {
		t.Fatalf("the standby forwards to %v, %v", u, ok)
	}
	first, err := b.store.CurrentLeader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := write(a, "t1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	if err := write(b, "t2"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the standby's write: %v, want ErrDeposed", err)
	}

	stalledAt := time.Now()
	a.lease.stalled.Store(true)
	select {
	case err := <-a.done:
		if !errors.Is(err, ErrLeadershipLost) {
			t.Fatalf("the stalled leader's Run = %v", err)
		}
		if since := time.Since(stalledAt); since > deadline+retry {
			t.Fatalf("the stalled leader stopped after %s, past its renew deadline", since)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled leader did not stop")
	}
	waitFor(t, "b to take over", b.elector.Status.Leading)
	if since := time.Since(stalledAt); since < lease-retry {
		t.Fatalf("b took over %s after the last renewal, before the lease (%s) ran out", since, lease)
	}
	second, err := b.store.CurrentLeader(context.Background())
	if err != nil || second.Holder != "b" || second.Epoch <= first.Epoch {
		t.Fatalf("lease after the takeover = %+v (before %+v), %v", second, first, err)
	}
	if err := write(a, "t3"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the old leader's write: %v, want ErrDeposed", err)
	}
	if err := write(b, "t3"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
}

// A leader that stops and releases the lease hands over at once, not after the lease's duration.
func TestPostgresReleaseHandsOverAtOnce(t *testing.T) {
	dsn := pgtest.DSN(t)
	const lease, deadline, retry = 30 * time.Second, 20 * time.Second, 100 * time.Millisecond
	a := startReplica(t, dsn, "a", lease, deadline, retry)
	waitFor(t, "a to lead", a.elector.Status.Leading)
	b := startReplica(t, dsn, "b", lease, deadline, retry)
	time.Sleep(3 * retry)
	a.cancel()
	if err := <-a.done; err != nil {
		t.Fatalf("Run after ctx ended: %v", err)
	}
	released := time.Now()
	if err := a.elector.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b to take over", b.elector.Status.Leading)
	if since := time.Since(released); since > 2*time.Second {
		t.Fatalf("b took over %s after the release", since)
	}
	if err := write(a, "t1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("a write after the release: %v, want ErrDeposed", err)
	}
}
