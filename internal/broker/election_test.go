package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// testElection is an election with timings short enough for a test and long enough for a loaded machine: a lease of
// 3 s, a leader that gives up renewing after 2 s, and a try every 200 ms.
var testElection = ElectionConfig{
	Namespace: "agent-kourier", Name: "agent-kourier-leader", Identity: "pod-a_1",
	LeaseDuration: 3 * time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: 200 * time.Millisecond,
}

// eventually polls cond until it holds or d passes, and then fails the test with what.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestElectionConfigDefaultsTheTimingsToTheDesignsAndChecksTheOrder(t *testing.T) {
	base := ElectionConfig{Namespace: "agent-kourier", Name: "leader", Identity: "pod"}
	got := base.withDefaults()
	if got.LeaseDuration != 15*time.Second || got.RenewDeadline != 10*time.Second || got.RetryPeriod != 2*time.Second {
		t.Errorf("defaults = %s, %s, %s; want 15s, 10s, 2s (D37)", got.LeaseDuration, got.RenewDeadline, got.RetryPeriod)
	}
	for name, tc := range map[string]struct {
		mutate func(*ElectionConfig)
		want   string
	}{
		"the defaults":                     {func(*ElectionConfig) {}, ""},
		"no identity":                      {func(c *ElectionConfig) { c.Identity = "" }, "Identity"},
		"no namespace and no name":         {func(c *ElectionConfig) { c.Namespace, c.Name = "", "" }, "[Name Namespace]"},
		"a lease no longer than a renewal": {func(c *ElectionConfig) { c.LeaseDuration = 10 * time.Second }, "lease duration"},
		"a renewal that leaves no retry":   {func(c *ElectionConfig) { c.RetryPeriod = 9 * time.Second }, "renew deadline"},
		"a negative retry period":          {func(c *ElectionConfig) { c.RetryPeriod = -time.Second }, "positive"},
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("validate = %v; want nil", err)
			case tc.want != "" && (!errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("validate = %v; want an ErrWiring saying %q", err, tc.want)
			}
		})
	}
}

// The process ends once, with one line, however many writes find the epoch stale at the same time.
func TestFailStopExitsOnceWithOneLine(t *testing.T) {
	var out bytes.Buffer
	var codes []int
	var mu sync.Mutex
	f := &failStop{log: slog.New(slog.NewTextHandler(&out, nil)), exit: func(c int) { mu.Lock(); codes = append(codes, c); mu.Unlock() }}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { f.now("a write found its epoch stale") })
	}
	wg.Wait()

	if len(codes) != 1 || codes[0] != ExitLeadershipLost {
		t.Errorf("exit calls = %v; want one, with %d", codes, ExitLeadershipLost)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 1 || !strings.Contains(out.String(), "a write found its epoch stale") {
		t.Errorf("log = %q; want one line with the reason", out.String())
	}
}

func TestStartGateAdmitsAStartUntilItIsClosedAndWaitsForOneInProgress(t *testing.T) {
	t.Run("a start in progress is aborted and waited for", func(t *testing.T) {
		g := &startGate{}
		leave, ok := g.enter()
		if !ok {
			t.Fatal("an open gate refused a start")
		}
		aborted, closed := make(chan struct{}), make(chan struct{})
		go func() {
			g.close(func() { close(aborted) })
			close(closed)
		}()
		select {
		case <-aborted:
		case <-time.After(5 * time.Second):
			t.Fatal("close did not abort the start in progress")
		}
		select {
		case <-closed:
			t.Fatal("close returned while a start was in progress")
		case <-time.After(50 * time.Millisecond):
		}
		if _, ok := g.enter(); ok {
			t.Error("a closed gate admitted a start")
		}
		leave()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("close did not return once the start had left")
		}
	})
	t.Run("nothing to abort when no start is running", func(t *testing.T) {
		g := &startGate{}
		leave, _ := g.enter()
		leave()
		g.close(func() {
			t.Error("abort was called with no start in progress: it would cancel the shutdown's own context")
		})
		if _, ok := g.enter(); ok {
			t.Error("a closed gate admitted a start")
		}
	})
}

// scriptedFence is a store.Fenced whose Lead answers from a script.
type scriptedFence struct {
	calls  atomic.Int32
	errs   []error // the errors of the first attempts; then it succeeds
	epoch  int64
	holder atomic.Value
}

func (f *scriptedFence) Lead(_ context.Context, holder string) (int64, error) {
	n := int(f.calls.Add(1))
	f.holder.Store(holder)
	if n <= len(f.errs) {
		return 0, f.errs[n-1]
	}
	return f.epoch, nil
}

func TestTakeEpochTriesAgainUntilTheStoreGivesOne(t *testing.T) {
	lock := errors.New("canceling statement due to lock timeout")
	f := &scriptedFence{errs: []error{lock, lock}, epoch: 7}

	epoch, err := takeEpoch(t.Context(), f, "pod-a", time.Millisecond, slog.New(slog.DiscardHandler))

	if err != nil || epoch != 7 || f.calls.Load() != 3 {
		t.Errorf("takeEpoch = %d, %v after %d attempts; want epoch 7 on the third", epoch, err, f.calls.Load())
	}
	if f.holder.Load() != "pod-a" {
		t.Errorf("Lead was given holder %v; want pod-a", f.holder.Load())
	}
}

func TestTakeEpochGivesUpWhenTheTimeIsUpAndSaysWhy(t *testing.T) {
	lock := errors.New("canceling statement due to lock timeout")
	f := &scriptedFence{errs: slicesOf(lock, 1000)}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := takeEpoch(ctx, f, "pod-a", 5*time.Millisecond, slog.New(slog.DiscardHandler))

	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, lock) {
		t.Errorf("takeEpoch = %v; want it to wrap the deadline and the last attempt's error", err)
	}
	if f.calls.Load() < 2 {
		t.Errorf("%d attempts; want it to have tried again", f.calls.Load())
	}
}

func TestTakeEpochDoesNotRetryAnInvalidArgument(t *testing.T) {
	f := &scriptedFence{errs: []error{store.ErrInvalid}}

	_, err := takeEpoch(t.Context(), f, "", time.Millisecond, slog.New(slog.DiscardHandler))

	if !errors.Is(err, store.ErrInvalid) || f.calls.Load() != 1 {
		t.Errorf("takeEpoch = %v after %d attempts; want ErrInvalid at once", err, f.calls.Load())
	}
}

func slicesOf(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}

// candidate is one candidacy of a test, with what its callbacks saw.
type candidate struct {
	c      *candidacy
	gained atomic.Int32
	lost   atomic.Int32
}

func standFor(t *testing.T, client *fake.Clientset, identity string) *candidate {
	t.Helper()
	cfg := testElection
	cfg.Identity = identity
	cand := &candidate{}
	c, err := startCandidacy(t.Context(), client, cfg, slog.New(slog.DiscardHandler), nil, candidacyCallbacks{
		gained: func(context.Context) { cand.gained.Add(1) },
		lost:   func() { cand.lost.Add(1) },
	})
	if err != nil {
		t.Fatalf("startCandidacy: %v", err)
	}
	cand.c = c
	t.Cleanup(func() { c.stop(context.WithoutCancel(t.Context())) })
	return cand
}

func holder(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	l, err := client.CoordinationV1().Leases(testElection.Namespace).Get(t.Context(), testElection.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the lease: %v", err)
	}
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

// One of two candidates wins the Lease, in Agent Kourier's namespace under the name the config gives; the other waits
// without ever being told it leads, and without losing anything.
func TestCandidacyOnlyTheWinnerIsToldItLeads(t *testing.T) {
	client := fake.NewClientset()
	a := standFor(t, client, "pod-a_1")
	eventually(t, 20*time.Second, "the first candidate was not told it leads", func() bool { return a.gained.Load() == 1 })
	b := standFor(t, client, "pod-b_1")

	time.Sleep(testElection.LeaseDuration + time.Second) // longer than a lease: a leader that renews keeps it

	if b.gained.Load() != 0 || a.gained.Load() != 1 || a.lost.Load() != 0 || b.lost.Load() != 0 {
		t.Errorf("gained a=%d b=%d, lost a=%d b=%d; want only a, once, and nothing lost", a.gained.Load(), b.gained.Load(), a.lost.Load(), b.lost.Load())
	}
	if got := holder(t, client); got != "pod-a_1" {
		t.Errorf("the Lease's holder = %q; want pod-a_1", got)
	}
}

// The chart makes the Lease, empty, and Agent Kourier is not allowed to create one: the first replica takes the empty Lease.
func TestCandidacyTakesTheEmptyLeaseTheChartMade(t *testing.T) {
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: testElection.Namespace, Name: testElection.Name}})
	client.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("Agent Kourier may not create a Lease")
	})

	a := standFor(t, client, "pod-a_1")

	eventually(t, 20*time.Second, "the candidate did not take the empty Lease", func() bool { return a.gained.Load() == 1 })
	if got := holder(t, client); got != "pod-a_1" {
		t.Errorf("the Lease's holder = %q; want pod-a_1", got)
	}
}

// A leader that cannot renew the Lease, here because the API server cannot be reached, gives up after the renew
// deadline and is told so, which is what ends the process, and before a candidate could take the lease.
func TestCandidacyALeaderThatCannotRenewTheLeaseIsToldSo(t *testing.T) {
	client := fake.NewClientset()
	var unreachable atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unreachable.Load() {
			return true, nil, errors.New("the API server is unreachable")
		}
		return false, nil, nil
	})
	a := standFor(t, client, "pod-a_1")
	eventually(t, 20*time.Second, "not leading", func() bool { return a.gained.Load() == 1 })

	start := time.Now()
	unreachable.Store(true)

	eventually(t, 10*time.Second, "the leader was not told it had lost the lease", func() bool { return a.lost.Load() == 1 })
	if took := time.Since(start); took < testElection.RenewDeadline-testElection.RetryPeriod || took > testElection.LeaseDuration {
		t.Errorf("the leader gave up after %s; want about the renew deadline, %s, and before the lease duration, %s", took, testElection.RenewDeadline, testElection.LeaseDuration)
	}
}

// The lease clock is told of the leader's renewals as the elector makes them, and only of those: while the leader renews
// the lease is never older than a retry period and a call; once the API server cannot be reached nothing renews it, and it
// ages by the real time that passes, until the elector gives up (the renew deadline) and the process is to end. This is the
// hook the chat fence stands on, tried on client-go's own elector and not on a stand-in.
func TestCandidacyTheLeaseClockFollowsTheLeadersAcceptedRenewals(t *testing.T) {
	client := fake.NewClientset()
	var unreachable atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unreachable.Load() {
			return true, nil, errors.New("the API server is unreachable")
		}
		return false, nil, nil
	})
	cfg := testElection
	cfg.Identity = "pod-a_1"
	lease := newLeaseClock(nil)
	var gained, lost atomic.Int32
	c, err := startCandidacy(t.Context(), client, cfg, slog.New(slog.DiscardHandler), lease, candidacyCallbacks{
		gained: func(context.Context) { gained.Add(1) }, lost: func() { lost.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.stop(context.WithoutCancel(t.Context())) })
	eventually(t, 20*time.Second, "not leading", func() bool { return gained.Load() == 1 })
	if _, ok := lease.since(); !ok {
		t.Fatal("the leader was told it leads, and the lease clock knows of no renewal: the acquisition is one")
	}

	for range 10 { // a leader that renews: never older than a retry period, a call and a margin for a loaded machine
		time.Sleep(cfg.RetryPeriod)
		if age, _ := lease.since(); age > cfg.RetryPeriod+cfg.RenewDeadline/2 {
			t.Fatalf("the lease clock says the last renewal began %s ago while the leader renews every %s", age, cfg.RetryPeriod)
		}
	}

	unreachable.Store(true)
	eventually(t, 10*time.Second, "the leader was not told it had lost the lease", func() bool { return lost.Load() == 1 })
	if age, _ := lease.since(); age < cfg.RenewDeadline-cfg.RetryPeriod {
		t.Errorf("the elector gave up %s after the last accepted renewal; want about the renew deadline, %s, which the chat fence uses as its bound", age, cfg.RenewDeadline)
	}
}

// A deliberate stop is not a loss, and hands the Lease over: the other replica takes it on its next try and does not
// wait out a lease duration.
func TestCandidacyAStopHandsTheLeaseOverWithoutALossAndWithoutAWait(t *testing.T) {
	client := fake.NewClientset()
	a := standFor(t, client, "pod-a_1")
	eventually(t, 20*time.Second, "not leading", func() bool { return a.gained.Load() == 1 })
	b := standFor(t, client, "pod-b_1")

	start := time.Now()
	a.c.stop(t.Context())

	eventually(t, 20*time.Second, "the other replica did not take the lease over", func() bool { return b.gained.Load() == 1 })
	if took := time.Since(start); took > testElection.LeaseDuration {
		t.Errorf("the hand-over took %s; want it quicker than the lease duration, %s", took, testElection.LeaseDuration)
	}
	if a.lost.Load() != 0 {
		t.Error("a deliberate stop was reported as a loss: it would end the process that is already stopping")
	}
	if got := holder(t, client); got != "pod-b_1" {
		t.Errorf("the Lease's holder = %q; want pod-b_1", got)
	}
}

func TestCandidacyAStandbyThatStopsIsNotALoss(t *testing.T) {
	client := fake.NewClientset()
	a := standFor(t, client, "pod-a_1")
	eventually(t, 20*time.Second, "not leading", func() bool { return a.gained.Load() == 1 })
	b := standFor(t, client, "pod-b_1")
	time.Sleep(500 * time.Millisecond)

	b.c.stop(t.Context())

	if b.lost.Load() != 0 || b.gained.Load() != 0 || holder(t, client) != "pod-a_1" {
		t.Errorf("lost=%d gained=%d holder=%q; want a standby's stop to change nothing", b.lost.Load(), b.gained.Load(), holder(t, client))
	}
}

func TestCandidacyRefusesTimingsClientGoWouldRefuse(t *testing.T) {
	cfg := testElection
	cfg.RenewDeadline = cfg.LeaseDuration
	_, err := startCandidacy(t.Context(), fake.NewClientset(), cfg, slog.New(slog.DiscardHandler), nil, candidacyCallbacks{gained: func(context.Context) {}, lost: func() {}})
	if !errors.Is(err, ErrWiring) {
		t.Errorf("startCandidacy = %v; want ErrWiring", err)
	}
}
