package interact_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// purges is a store that only counts the purges and clears it is asked for. failFirst of the purges fail, and
// failClear makes every clear fail.
type purges struct {
	mu        sync.Mutex
	got       []purge
	cleared   []clearing
	failFirst int
	failClear bool
}

type purge struct {
	now   time.Time
	grace time.Duration
}

type clearing struct {
	now                     time.Time
	grace, undeliveredGrace time.Duration
}

func (p *purges) ClearSettledInteractions(_ context.Context, now time.Time, grace, undeliveredGrace time.Duration) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleared = append(p.cleared, clearing{now, grace, undeliveredGrace})
	if p.failClear {
		return 0, errBoom
	}
	return 0, nil
}

func (p *purges) clears() []clearing {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]clearing(nil), p.cleared...)
}

func (p *purges) PurgePresses(_ context.Context, now time.Time, grace time.Duration) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, purge{now, grace})
	if len(p.got) <= p.failFirst {
		return 0, errBoom
	}
	return 0, nil
}

func (p *purges) list() []purge {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]purge(nil), p.got...)
}

func startRetention(t *testing.T, cfg interact.RetentionConfig) (stop func() error) {
	t.Helper()
	rt, err := interact.NewRetention(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	res := make(chan error, 1)
	go func() { res <- rt.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() { cancel(); runErr = <-res })
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// TestRetentionPurgesOnEveryTickWithItsGraceAndTheClock: the backstop asks the store, again and again, to
// delete the presses past their interaction's deadline plus the grace, as of courier's own clock.
func TestRetentionPurgesOnEveryTickWithItsGraceAndTheClock(t *testing.T) {
	p := &purges{}
	startRetention(t, interact.RetentionConfig{Store: p, Interval: 5 * time.Millisecond, Grace: 90 * time.Minute, Now: func() time.Time { return d0 },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	waitFor(t, "three purges", func() bool { return len(p.list()) >= 3 })
	for _, got := range p.list() {
		if got.now != d0 || got.grace != 90*time.Minute {
			t.Errorf("purge = %+v; want the clock's now and the configured grace", got)
		}
	}
}

// TestRetentionClearsSettledInteractionsOnEveryTickWithItsGracesAndTheClock: beside the presses, the loop asks the
// store to clear what settled interactions hold, with the configured graces, as of courier's own clock.
func TestRetentionClearsSettledInteractionsOnEveryTickWithItsGracesAndTheClock(t *testing.T) {
	p := &purges{}
	startRetention(t, interact.RetentionConfig{Store: p, Interval: 5 * time.Millisecond, ClearGrace: 30 * time.Hour, UndeliveredGrace: 50 * time.Hour,
		Now: func() time.Time { return d0 }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	waitFor(t, "three clears", func() bool { return len(p.clears()) >= 3 })
	for _, got := range p.clears() {
		if got != (clearing{d0, 30 * time.Hour, 50 * time.Hour}) {
			t.Errorf("clear = %+v; want the clock's now and the configured graces", got)
		}
	}
}

// TestRetentionClearsAfterTheGraceAndNotBefore: on the real store, a settled interaction keeps its request
// and answer until its grace has passed since it was decided, and then has neither, and keeps its row.
func TestRetentionClearsAfterTheGraceAndNotBefore(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if _, err := r.process(t, r.submit(t, "UALICE1", 0)); err != nil { // answered at r.now, and delivered
		t.Fatal(err)
	}
	decided := r.row(t)
	if len(decided.Request) == 0 || len(decided.Answer) == 0 || decided.DeliveredAt.IsZero() {
		t.Fatalf("answered row %+v; want a request, an answer and a delivery", decided)
	}
	var now atomic.Int64
	now.Store(r.now.Add(time.Hour - time.Second).UnixNano())
	var tick atomic.Int32
	logs := &logBuf{}
	startRetention(t, interact.RetentionConfig{Store: r.st, Interval: 5 * time.Millisecond, ClearGrace: time.Hour, UndeliveredGrace: time.Hour,
		Now: func() time.Time { tick.Add(1); return time.Unix(0, now.Load()).UTC() }, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	waitFor(t, "ticks inside the grace", func() bool { return tick.Load() >= 5 })
	if got := r.row(t); len(got.Request) == 0 || len(got.Answer) == 0 {
		t.Fatalf("row %+v a second inside the grace; want it kept whole", got)
	}
	now.Store(r.now.Add(time.Hour).UnixNano())
	waitFor(t, "the row to be cleared", func() bool { return len(r.row(t).Request) == 0 })
	got := r.row(t)
	want := decided
	want.Request, want.Answer = nil, nil
	if len(got.Answer) != 0 || got.State != want.State || got.Decider != want.Decider || !got.DecidedAt.Equal(want.DecidedAt) || got.MessageRef != want.MessageRef || got.ID != want.ID {
		t.Errorf("cleared row %+v; want only the request and answer gone: %+v", got, want)
	}
	waitFor(t, "the clearing to be logged", func() bool { return strings.Contains(logs.String(), "cleared=1") })
}

// TestRetentionDoesNotPurgeBeforeItsFirstTick: nothing is deleted at once; a restart does not purge ahead of
// the worker's recovery of the presses it left.
func TestRetentionDoesNotPurgeBeforeItsFirstTick(t *testing.T) {
	p := &purges{}
	stop := startRetention(t, interact.RetentionConfig{Store: p, Interval: time.Hour})
	time.Sleep(20 * time.Millisecond)
	_ = stop()
	if got := p.list(); len(got) != 0 {
		t.Errorf("purges %+v before the first tick; want none", got)
	}
}

// TestRetentionKeepsPressesWithinTheGraceAndPurgesThemAfter: on the real store, a press whose interaction's
// deadline passed less than the grace ago is kept, since a worker may still be on it or the sweep still
// waiting for it; one past it is deleted.
func TestRetentionKeepsPressesWithinTheGraceAndPurgesThemAfter(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(deadline.Add(time.Hour - time.Second).UnixNano())
	var tick atomic.Int32
	startRetention(t, interact.RetentionConfig{Store: r.st, Interval: 5 * time.Millisecond, Grace: time.Hour,
		Now: func() time.Time { tick.Add(1); return time.Unix(0, now.Load()).UTC() }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	waitFor(t, "purges", func() bool { return tick.Load() >= 5 })
	if r.waiting(t) != 1 {
		t.Fatalf("%d presses waiting a second inside the grace; want it kept", r.waiting(t))
	}
	now.Store(deadline.Add(time.Hour).UnixNano())
	waitFor(t, "the press to be purged", func() bool { return r.waiting(t) == 0 })
}

// TestRetentionLogsAFailedPurgeAndGoesOn: the backstop is only a backstop, so a store that fails for a while
// costs a log line each time, and the next tick tries again.
func TestRetentionLogsAFailedPurgeAndGoesOn(t *testing.T) {
	p := &purges{failFirst: 2}
	logs := &logBuf{}
	startRetention(t, interact.RetentionConfig{Store: p, Interval: 5 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	waitFor(t, "purges after the failures", func() bool { return len(p.list()) >= 4 })
	if out := logs.String(); strings.Count(out, "level=WARN") != 2 || !strings.Contains(out, "boom") {
		t.Errorf("log = %q; want one warning with the error for each of the two failures", out)
	}
}

// countingPurger counts the purges it passes on, so a test can wait for ticks instead of sleeping.
type countingPurger struct {
	interact.RetentionStore
	n atomic.Int32
}

func (c *countingPurger) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error) {
	n, err := c.RetentionStore.PurgePresses(ctx, now, grace)
	c.n.Add(1)
	return n, err
}

// TestRetentionLogsWhatItPurged: the count is logged when there is one, and only then.
func TestRetentionLogsWhatItPurged(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	counted := &countingPurger{RetentionStore: r.st}
	startRetention(t, interact.RetentionConfig{Store: counted, Interval: 5 * time.Millisecond, Grace: time.Hour, Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Now: func() time.Time { return deadline.Add(2 * time.Hour) }})
	waitFor(t, "the purge to be logged", func() bool { return strings.Contains(logs.String(), "purged=1") })
	seen := counted.n.Load()
	waitFor(t, "two more ticks, which purge nothing", func() bool { return counted.n.Load() >= seen+2 })
	if out := logs.String(); strings.Count(out, "level=INFO") != 1 || !strings.Contains(out, "purged=1") {
		t.Errorf("log = %q; want the purge logged once, with its count", out)
	}
}

// TestRetentionEndsWithItsContextAndLeavesNoGoroutine.
func TestRetentionEndsWithItsContextAndLeavesNoGoroutine(t *testing.T) {
	before := goleak.IgnoreCurrent()
	p := &purges{}
	stop := startRetention(t, interact.RetentionConfig{Store: p, Interval: 5 * time.Millisecond})
	waitFor(t, "a purge", func() bool { return len(p.list()) >= 1 })
	if err := stop(); err == nil {
		t.Fatal("Run returned nil; want the context's error")
	}
	n := len(p.list())
	time.Sleep(30 * time.Millisecond)
	if got := len(p.list()); got != n {
		t.Errorf("%d purges after Run returned; want none", got-n)
	}
	goleak.VerifyNone(t, before)
}

func TestNewRetentionNeedsAStore(t *testing.T) {
	if _, err := interact.NewRetention(interact.RetentionConfig{}); err == nil {
		t.Error("NewRetention accepted no Store")
	}
}

// TestAFailedClearIsLoggedAndDoesNotStopThePurge: the two sweeps are independent, and each is tried again at the
// next tick.
func TestAFailedClearIsLoggedAndDoesNotStopThePurge(t *testing.T) {
	p := &purges{failClear: true}
	logs := &logBuf{}
	startRetention(t, interact.RetentionConfig{Store: p, Interval: 5 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	waitFor(t, "purges and clears", func() bool { return len(p.list()) >= 3 && len(p.clears()) >= 3 })
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "could not be cleared") || strings.Contains(out, "could not be purged") {
		t.Errorf("log = %q; want warnings for the clears, and none for the purges", out)
	}
}

// purgingDecider purges the presses, as the retention loop would, in the middle of the worker's work on one.
type purgingDecider struct {
	st    store.Presses
	inner interact.PressDecider
	at    time.Time
}

func (p purgingDecider) Decide(ctx context.Context, pd interact.PressDecision) error {
	if _, err := p.st.PurgePresses(ctx, p.at, time.Hour); err != nil {
		return err
	}
	return p.inner.Decide(ctx, pd)
}

// TestPurgingAPressTheWorkerIsOnIsHarmless: the worker holds the press it is on in memory, and DeletePress
// of one that is already gone is not an error. So a purge that takes the press between the worker's read
// and its finish changes nothing: the press is decided once, the answer sent once, and Process is Done.
func TestPurgingAPressTheWorkerIsOnIsHarmless(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if err := r.pl.Press(t.Context(), r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	presses, _ := r.st.ListPresses(t.Context())
	pl, err := interact.NewPipeline(interact.PressConfig{Store: r.st, Sessions: r.st, Lookup: lookup{interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}},
		Authorizer: r.az, Ephemerals: r.eph, Auditor: r.aud, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return r.now },
		Decider: purgingDecider{st: r.st, inner: r.d, at: deadline.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := pl.Process(t.Context(), presses[0]); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want it done, though the press was purged under it", res, err)
	}
	if r.waiting(t) != 0 || r.row(t).State != store.InteractionAnswered || len(r.sess.answers()) != 1 || len(r.eph.list()) != 0 {
		t.Errorf("%d waiting, state %s, %d sends, told %q; want the press gone, answered once and no one told", r.waiting(t), r.row(t).State, len(r.sess.answers()), r.eph.list())
	}
}

// TestRetentionCountsWhatItCleared: the count goes to the hook, so that it can be a metric, and only when there is one.
func TestRetentionCountsWhatItCleared(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if _, err := r.process(t, r.submit(t, "UALICE1", 0)); err != nil {
		t.Fatal(err)
	}
	var cleared, calls atomic.Int32
	startRetention(t, interact.RetentionConfig{Store: r.st, Interval: 5 * time.Millisecond, ClearGrace: time.Hour, UndeliveredGrace: time.Hour,
		Now:       func() time.Time { return r.now.Add(2 * time.Hour) },
		OnCleared: func(n int) { calls.Add(1); cleared.Add(int32(n)) }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	waitFor(t, "the clearing to be counted", func() bool { return cleared.Load() == 1 })
	time.Sleep(30 * time.Millisecond) // more ticks, which clear nothing
	if cleared.Load() != 1 || calls.Load() != 1 {
		t.Errorf("counted %d in %d calls; want 1 in one: a pass that clears nothing is not reported", cleared.Load(), calls.Load())
	}
}
