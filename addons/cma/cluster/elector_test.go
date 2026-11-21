package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

// fakeLease is one replica's view of a lease held in memory. other is the holder that leads when this one does not;
// renewErr, when set, is what every Renew returns.
type fakeLease struct {
	mu       sync.Mutex
	other    *store.Lease
	held     bool
	renewErr error
	renewals int
	released bool
}

func (f *fakeLease) TryLead(_ context.Context, holder, address string, _ time.Duration) (store.Lease, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.other != nil && f.other.Live {
		return *f.other, false, nil
	}
	f.held = true
	return store.Lease{Holder: holder, Address: address, Epoch: 1, Live: true}, true, nil
}

func (f *fakeLease) Renew(context.Context, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals++
	return f.renewErr
}

func (f *fakeLease) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = true
	return nil
}

func (f *fakeLease) CurrentLeader(context.Context) (store.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.other != nil {
		return *f.other, nil
	}
	return store.Lease{}, nil
}

func newElector(l Lease, holder string) *Elector {
	return &Elector{Lease: l, Holder: holder, Address: "http://" + holder + ":8090", LeaseDuration: 300 * time.Millisecond,
		RenewDeadline: 200 * time.Millisecond, RetryPeriod: 50 * time.Millisecond, Status: &Status{}, Log: discard()}
}

// run starts e.Run with lead and returns its result's channel.
func run(ctx context.Context, e *Elector, lead func(context.Context) error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, lead) }()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestElectorLeadsAndRenews(t *testing.T) {
	lease := &fakeLease{}
	e := newElector(lease, "a")
	e.Telemetry = newTelemetry(t)
	ctx, cancel := context.WithCancel(t.Context())
	led := make(chan struct{})
	done := run(ctx, e, func(ctx context.Context) error {
		close(led)
		<-ctx.Done()
		return nil
	})
	<-led
	if !e.Status.Leading() || e.Status.Serving() {
		t.Fatalf("leading %v serving %v: the leader serves only once its takeover says so", e.Status.Leading(), e.Status.Serving())
	}
	waitFor(t, "renewals", func() bool { lease.mu.Lock(); defer lease.mu.Unlock(); return lease.renewals >= 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after ctx ended: %v", err)
	}
	if lease.released {
		t.Fatal("Run released the lease itself: the caller does, once it has stopped serving")
	}
	if err := e.Release(t.Context()); err != nil || !lease.released {
		t.Fatalf("Release: %v, released %v", err, lease.released)
	}
	if got := lineValue(metrics(t, e), "agentkourier_cma_leader_takeovers_total"); got != "1" {
		t.Fatalf("takeovers = %q", got)
	}
}

func metrics(t *testing.T, e *Elector) string {
	t.Helper()
	return scrape(t, e.Telemetry)
}

// A standby campaigns without leading while another holds a live lease, and knows where it is.
func TestElectorWaitsWhileAnotherLeads(t *testing.T) {
	lease := &fakeLease{other: &store.Lease{Holder: "b", Address: "http://b:8090", Epoch: 4, Live: true}}
	e := newElector(lease, "a")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, func(context.Context) error {
		t.Error("a standby ran the leader's work")
		return nil
	})
	if err != nil || e.Status.Leading() {
		t.Fatalf("Run = %v, leading %v", err, e.Status.Leading())
	}
	u, ok := e.Leader(t.Context())
	if !ok || u.String() != "http://b:8090" {
		t.Fatalf("Leader = %v, %v", u, ok)
	}
	if err := e.Release(t.Context()); err != nil || lease.released {
		t.Fatal("a standby released a lease it never held")
	}
}

// Leader never names this replica, or a lease that has expired.
func TestElectorLeaderIsAnotherLiveReplica(t *testing.T) {
	for name, l := range map[string]store.Lease{
		"self":    {Holder: "a", Address: "http://a:8090", Live: true},
		"expired": {Holder: "b", Address: "http://b:8090"},
		"none":    {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newElector(&fakeLease{other: &l}, "a")
			if u, ok := e.Leader(t.Context()); ok {
				t.Fatalf("Leader = %v", u)
			}
		})
	}
}

// A renewal that finds the lease taken stops the leader at once.
func TestElectorStopsWhenDeposed(t *testing.T) {
	lease := &fakeLease{renewErr: store.ErrDeposed}
	e := newElector(lease, "a")
	e.RenewDeadline = time.Hour
	e.LeaseDuration = 2 * time.Hour
	start := time.Now()
	err := <-run(t.Context(), e, func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, ErrLeadershipLost) || time.Since(start) > time.Second {
		t.Fatalf("Run = %v after %s", err, time.Since(start))
	}
}

// Renewals that fail stop the leader once none has succeeded for the renew deadline, and not before.
func TestElectorStopsAtTheRenewDeadline(t *testing.T) {
	lease := &fakeLease{renewErr: errors.New("connection refused")}
	e := newElector(lease, "a")
	start := time.Now()
	err := <-run(t.Context(), e, func(ctx context.Context) error { <-ctx.Done(); return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("Run = %v", err)
	}
	if elapsed < e.RenewDeadline || elapsed > e.LeaseDuration {
		t.Fatalf("stopped after %s; want between the renew deadline (%s) and the lease (%s)", elapsed, e.RenewDeadline, e.LeaseDuration)
	}
}

// A takeover that fails (CMA cannot be read) ends Run with its error.
func TestElectorReturnsTheTakeoversError(t *testing.T) {
	boom := errors.New("list sessions: 503")
	err := <-run(t.Context(), newElector(&fakeLease{}, "a"), func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v", err)
	}
}
