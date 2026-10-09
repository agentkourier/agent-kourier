package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

const (
	digestBinding = "sre/sre-alerts"
	digestConn    = "agent-kourier-system/slack-main"
	digestChannel = "C0EXAMPLE01"
)

var digestNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// digestWorld is a digest pass over a real store, whose queue records the posts it is given as the outbox would be.
type digestWorld struct {
	st    *sqlite.Store
	q     *recordingQueue
	d     *triggerDigest
	logs  *logBuffer
	mu    sync.Mutex
	clock time.Time
}

// recordingQueue is the outbox: it records the posts, and keeps one row for each ID as the outbox's store does.
type recordingQueue struct {
	mu    sync.Mutex
	posts []outbox.Post
	rows  map[string]bool
	fail  error
}

func (q *recordingQueue) Enqueue(_ context.Context, p outbox.Post) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail != nil {
		return "", q.fail
	}
	id := outbox.ID(p.Source, p.Role)
	if q.rows == nil {
		q.rows = map[string]bool{}
	}
	q.rows[id] = true
	q.posts = append(q.posts, p)
	return id, nil
}

func (q *recordingQueue) got() []outbox.Post {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.posts)
}

// markFails wraps the store so that marking a window digested fails the first n times.
type markFails struct {
	digestStore
	n int
}

func (m *markFails) MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) error {
	if m.n > 0 {
		m.n--
		return errors.New("disk full")
	}
	return m.digestStore.MarkTriggerDigested(ctx, binding, trigger, start)
}

func newDigestWorld(t *testing.T, wrap func(digestStore) digestStore) *digestWorld {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	w := &digestWorld{st: st, q: &recordingQueue{}, logs: &logBuffer{}, clock: digestNow}
	var ds digestStore = st
	if wrap != nil {
		ds = wrap(ds)
	}
	w.d, err = newTriggerDigest(triggerDigestConfig{Store: ds, Queue: w.q, Now: w.now, Logger: slog.New(slog.NewTextHandler(w.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *digestWorld) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.clock
}

func (w *digestWorld) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clock = w.clock.Add(d)
}

// stop records a start that a limit stopped, at the world's time, in a window of 10 minutes.
func (w *digestWorld) stop(t *testing.T, trigger int, ts string, lim store.TriggerRunLimits) {
	t.Helper()
	run := store.TriggerRun{Binding: digestBinding, Trigger: trigger, At: w.now(), Key: store.ConversationKey{Connection: digestConn, Channel: digestChannel, Thread: ts}}
	lim.DigestWindow = 10 * time.Minute
	if _, err := w.st.AdmitTriggerRun(t.Context(), run, lim); err != nil {
		t.Fatal(err)
	}
}

// onePlace is a limit of one start an hour: the first start of a trigger goes ahead and the rest are stopped.
var onePlace = store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour}

// storm stops n starts of trigger 0, after the one that went ahead.
func (w *digestWorld) storm(t *testing.T, n int) {
	t.Helper()
	for i := range n + 1 {
		w.stop(t, 0, fmt.Sprintf("%d.1", 100+i), onePlace)
	}
}

// The line says how many alerts were not investigated, in how long and why, and nothing else: it is posted at the top
// level of the Binding's channel, through the outbox, once the window has ended.
func TestTheDigestIsOneLineInTheChannelWhenTheWindowEnds(t *testing.T) {
	w := newDigestWorld(t, nil)
	w.storm(t, 7)

	w.advance(10*time.Minute - time.Nanosecond)
	w.d.pass(t.Context())
	if got := w.q.got(); len(got) != 0 {
		t.Fatalf("a digest was posted before its window ended: %+v", got)
	}

	w.advance(time.Nanosecond)
	w.d.pass(t.Context())
	got := w.q.got()
	if len(got) != 1 {
		t.Fatalf("posts = %+v; want one digest", got)
	}
	p := got[0]
	if p.Role != store.OutboxDigest || p.Channel != digestChannel || p.Thread != "" {
		t.Errorf("post = %+v; want a digest at the top level of %s", p, digestChannel)
	}
	if want := "7 alert messages not investigated in the last 10m (rate limit)."; p.Out.Message == nil || p.Out.Message.Fallback != want {
		t.Errorf("the line = %+v; want %q", p.Out.Message, want)
	}
	if err := p.Out.Validate(); err != nil {
		t.Errorf("the digest is not a valid message: %v", err)
	}
}

// A window is digested once: further passes post nothing, and a failure of the mark, which a crash between the post and
// the mark is, posts the same row again, so the outbox holds one.
func TestTheDigestIsPostedOncePerWindow(t *testing.T) {
	var marks *markFails
	w := newDigestWorld(t, func(s digestStore) digestStore { marks = &markFails{digestStore: s, n: 2}; return marks })
	w.storm(t, 3)
	w.advance(11 * time.Minute)

	for range 4 { // the mark fails twice, then holds
		w.d.pass(t.Context())
	}
	posts := w.q.got()
	if len(posts) != 3 {
		t.Fatalf("%d posts over four passes with two failed marks; want three asks, the third marking it", len(posts))
	}
	ids := map[string]bool{}
	for _, p := range posts {
		ids[outbox.ID(p.Source, p.Role)] = true
	}
	if len(ids) != 1 || len(w.q.rows) != 1 {
		t.Errorf("the asks name IDs %v; want one row for the window, so one digest", ids)
	}
	if !strings.Contains(w.logs.String(), "could not be recorded") {
		t.Errorf("the log %q; want a warning for the failed mark", w.logs.String())
	}
}

// A later burst is a window of its own, with a line of its own, and a digest names the windows of other triggers
// separately.
func TestEachWindowAndTriggerHasItsOwnDigest(t *testing.T) {
	w := newDigestWorld(t, nil)
	w.storm(t, 2)
	w.stop(t, 1, "200.1", onePlace)
	w.stop(t, 1, "201.1", onePlace)
	w.advance(11 * time.Minute)
	w.d.pass(t.Context())
	w.stop(t, 0, "300.1", onePlace) // trigger 0 is still inside its hour: this is stopped
	w.advance(11 * time.Minute)
	w.d.pass(t.Context())
	w.d.pass(t.Context())

	posts := w.q.got()
	var lines []string
	ids := map[string]bool{}
	for _, p := range posts {
		lines = append(lines, p.Out.Message.Fallback)
		ids[outbox.ID(p.Source, p.Role)] = true
	}
	slices.Sort(lines)
	want := []string{
		"1 alert message not investigated in the last 10m (rate limit).",
		"1 alert message not investigated in the last 10m (rate limit).",
		"2 alert messages not investigated in the last 10m (rate limit).",
	}
	if !slices.Equal(lines, want) || len(ids) != 3 {
		t.Errorf("lines %q with IDs %v; want three digests with three IDs", lines, ids)
	}
}

func TestTheDigestSaysWhyAndHowLong(t *testing.T) {
	for _, tt := range []struct {
		name string
		d    store.TriggerDigest
		want string
	}{
		{"one alert", digestOf(10*time.Minute, store.LimitRate, 1), "1 alert message not investigated in the last 10m (rate limit)."},
		{"several", digestOf(10*time.Minute, store.LimitRate, 7), "7 alert messages not investigated in the last 10m (rate limit)."},
		{"concurrency", digestOf(10*time.Minute, store.LimitConcurrent, 2), "2 alert messages not investigated in the last 10m (concurrency limit)."},
		{"daily", digestOf(time.Hour, store.LimitDaily, 4), "4 alert messages not investigated in the last 1h (daily cap)."},
		{"hours and minutes", digestOf(90*time.Minute, store.LimitRate, 2), "2 alert messages not investigated in the last 1h30m (rate limit)."},
		{"seconds", digestOf(30*time.Second, store.LimitRate, 2), "2 alert messages not investigated in the last 30s (rate limit)."},
		{"a day", digestOf(24*time.Hour, store.LimitDaily, 2), "2 alert messages not investigated in the last 24h (daily cap)."},
		{"two reasons, in a fixed order", store.TriggerDigest{Start: digestNow, End: digestNow.Add(10 * time.Minute),
			Counts: map[store.TriggerLimit]int{store.LimitDaily: 1, store.LimitConcurrent: 2, store.LimitRate: 3}},
			"6 alert messages not investigated in the last 10m (rate limit: 3, concurrency limit: 2, daily cap: 1)."},
	} {
		if got := digestText(tt.d); got != tt.want {
			t.Errorf("%s: %q; want %q", tt.name, got, tt.want)
		}
	}
}

func digestOf(window time.Duration, limit store.TriggerLimit, n int) store.TriggerDigest {
	return store.TriggerDigest{Start: digestNow, End: digestNow.Add(window), Counts: map[store.TriggerLimit]int{limit: n}}
}

// The digest names no alert: its text is the count, the window and the limit, whatever the alerts said.
func TestTheDigestNamesNoAlert(t *testing.T) {
	w := newDigestWorld(t, nil)
	w.storm(t, 4)
	w.advance(11 * time.Minute)
	w.d.pass(t.Context())
	for _, p := range w.q.got() {
		if strings.ContainsAny(p.Out.Message.Fallback, "[]<>@") || strings.Contains(strings.ToLower(p.Out.Message.Fallback), "kube") {
			t.Errorf("digest %q looks like it holds alert text", p.Out.Message.Fallback)
		}
		for _, id := range []string{p.Source, p.Channel, p.Thread} {
			if strings.Contains(id, ".1") {
				t.Errorf("digest %+v is tied to an alert message", p)
			}
		}
	}
	if logs := w.logs.String(); strings.Contains(logs, "100.1") {
		t.Errorf("the log names an alert: %s", logs)
	}
}

// A digest that cannot be queued is tried again at the next pass and is not marked.
func TestADigestThatCannotBeQueuedIsTriedAgain(t *testing.T) {
	w := newDigestWorld(t, nil)
	w.storm(t, 2)
	w.advance(11 * time.Minute)
	w.q.fail = errors.New("the database is locked")
	w.d.pass(t.Context())
	w.q.mu.Lock()
	w.q.fail = nil
	w.q.mu.Unlock()
	w.d.pass(t.Context())
	if got := w.q.got(); len(got) != 1 {
		t.Errorf("posts = %+v; want the digest queued once the queue works", got)
	}
}

// purgeSpy wraps the store and records the cutoffs it is asked to purge runs at.
type purgeSpy struct {
	digestStore
	cutoffs []time.Time
}

func (p *purgeSpy) PurgeTriggerRuns(ctx context.Context, before time.Time) (int, error) {
	p.cutoffs = append(p.cutoffs, before)
	return p.digestStore.PurgeTriggerRuns(ctx, before)
}

// The runs older than a day, the longest window any limit counts, are purged, and at most once an hour. A window that
// ended unseen is digested in the pass that purges its rows, not lost to it.
func TestTheDigestLoopPurgesRunsOlderThanADayOncePerHour(t *testing.T) {
	var spy *purgeSpy
	w := newDigestWorld(t, func(s digestStore) digestStore { spy = &purgeSpy{digestStore: s}; return spy })
	w.storm(t, 2)
	w.advance(25 * time.Hour)
	w.d.pass(t.Context())
	if len(w.q.got()) != 1 {
		t.Errorf("posts = %+v; want the digest of the window that ended a day ago, before its rows are purged", w.q.got())
	}
	// Each purge is asked to delete what is older than the longest window any limit counts, from the time of its own
	// pass: not older than that, which would keep rows nothing counts, and not newer, which would delete starts that
	// the daily cap and the concurrency still count.
	wantCutoffs := []time.Time{w.now().Add(-v1alpha1.MaxLimitWindow)}
	for _, step := range []struct {
		after time.Duration
		purge bool
	}{{30 * time.Minute, false}, {31 * time.Minute, true}, {time.Minute, false}} {
		w.advance(step.after)
		before := len(spy.cutoffs)
		w.d.pass(t.Context())
		if purged := len(spy.cutoffs) > before; purged != step.purge {
			t.Errorf("a pass %v after the last: purged = %v; want %v", step.after, purged, step.purge)
		}
		if step.purge {
			wantCutoffs = append(wantCutoffs, w.now().Add(-v1alpha1.MaxLimitWindow))
		}
	}
	if !slices.EqualFunc(spy.cutoffs, wantCutoffs, time.Time.Equal) {
		t.Errorf("purged runs from before %v; want %v: a day before each pass", spy.cutoffs, wantCutoffs)
	}
}

func TestADigestLoopNeedsAStoreAndAQueue(t *testing.T) {
	if _, err := newTriggerDigest(triggerDigestConfig{Queue: &recordingQueue{}}); err == nil {
		t.Error("newTriggerDigest with no Store succeeded")
	}
	if _, err := newTriggerDigest(triggerDigestConfig{Store: &sqlite.Store{}}); err == nil {
		t.Error("newTriggerDigest with no Queue succeeded")
	}
}

// Run passes on every tick until its context ends, and returns its error.
func TestTheDigestLoopRunsUntilItsContextEnds(t *testing.T) {
	w := newDigestWorld(t, nil)
	w.storm(t, 2)
	w.advance(11 * time.Minute)
	d, err := newTriggerDigest(triggerDigestConfig{Store: w.st, Queue: w.q, Interval: time.Millisecond, Now: w.now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); len(w.q.got()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the loop posted no digest")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want context.Canceled", err)
	}
	if len(w.q.got()) != 1 {
		t.Errorf("posts = %+v; want one digest however often it passed", w.q.got())
	}
}
