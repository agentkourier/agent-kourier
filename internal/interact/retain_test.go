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

// purges is a press store that only counts the purges it is asked for. failFirst of them fail.
type purges struct {
	mu        sync.Mutex
	got       []purge
	failFirst int
}

type purge struct {
	now   time.Time
	grace time.Duration
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
	interact.PressPurger
	n atomic.Int32
}

func (c *countingPurger) PurgePresses(ctx context.Context, now time.Time, grace time.Duration) (int, error) {
	n, err := c.PressPurger.PurgePresses(ctx, now, grace)
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
	counted := &countingPurger{PressPurger: r.st}
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
