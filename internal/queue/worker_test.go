package queue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

const (
	lease       = 30 * time.Second
	retryDelay  = time.Minute
	maxAttempts = 3
)

// fakeClock is single-goroutine: RunOnce and its handlers run on the test goroutine.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fixture struct {
	ctx   context.Context
	store *sqlite.Store
	clock *fakeClock
	log   *bytes.Buffer
	w     *Worker
}

// newFixture builds a Worker over a temp-dir SQLite store with a fixed backoff
// of retryDelay and a fake clock at t0.
func newFixture(t *testing.T, h HandlerFunc, mod ...func(*Config)) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "courier.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{ctx: ctx, store: s, clock: &fakeClock{t: t0}, log: &bytes.Buffer{}}
	cfg := Config{
		Queue: s, Handler: h, Lease: lease, MaxAttempts: maxAttempts,
		Backoff: func(int) time.Duration { return retryDelay },
		Now:     f.clock.Now,
		Logger:  slog.New(slog.NewTextHandler(f.log, nil)),
	}
	for _, m := range mod {
		m(&cfg)
	}
	if f.w, err = New(cfg); err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func (f *fixture) append(t *testing.T, id string) {
	t.Helper()
	e := store.Event{ID: id, Kind: "alert", BindingKey: "ns/b", Payload: []byte("p"), ReceivedAt: t0}
	if _, err := f.store.AppendEvent(f.ctx, e); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func (f *fixture) get(t *testing.T, id string) store.Event {
	t.Helper()
	e, err := f.store.GetEvent(f.ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	return e
}

func (f *fixture) runOnce(t *testing.T, wantHandled bool) {
	t.Helper()
	handled, err := f.w.RunOnce(f.ctx)
	if err != nil || handled != wantHandled {
		t.Fatalf("RunOnce = %v, %v; want %v, nil", handled, err, wantHandled)
	}
}

func TestRunOnceHandlesAndCompletes(t *testing.T) {
	var seen []store.Event
	f := newFixture(t, func(_ context.Context, e store.Event) error { seen = append(seen, e); return nil })
	f.runOnce(t, false) // empty queue
	f.append(t, "ev")
	f.runOnce(t, true)

	if len(seen) != 1 || seen[0].ID != "ev" || seen[0].Attempts != 1 || string(seen[0].Payload) != "p" {
		t.Fatalf("handler saw %+v; want one claimed event at attempt 1", seen)
	}
	if got := f.get(t, "ev"); got.Status != store.EventDone {
		t.Fatalf("status = %q; want done", got.Status)
	}
	f.runOnce(t, false)
}

func TestRunOnceRetriesWithBackoffThenSucceeds(t *testing.T) {
	calls := 0
	var backoffArgs []int
	f := newFixture(t, func(context.Context, store.Event) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}, func(c *Config) {
		c.Backoff = func(attempt int) time.Duration { backoffArgs = append(backoffArgs, attempt); return retryDelay }
	})
	f.append(t, "ev")
	f.runOnce(t, true)

	got := f.get(t, "ev")
	if got.Status != store.EventPending || got.LastError != "boom" || !got.NextAttemptAt.Equal(t0.Add(retryDelay)) {
		t.Fatalf("after failure = %+v; want pending at %v with last error boom", got, t0.Add(retryDelay))
	}
	f.runOnce(t, false) // not due yet
	f.clock.advance(retryDelay)
	f.runOnce(t, true)

	if got = f.get(t, "ev"); got.Status != store.EventDone || got.Attempts != 2 || calls != 2 {
		t.Fatalf("after retry = %+v, %d calls; want done at attempt 2", got, calls)
	}
	if len(backoffArgs) != 1 || backoffArgs[0] != 1 {
		t.Fatalf("backoff called with %v; want [1]", backoffArgs)
	}
}

func TestRunOnceDeadLettersAfterMaxAttempts(t *testing.T) {
	calls := 0
	f := newFixture(t, func(context.Context, store.Event) error { calls++; return errors.New("always") })
	f.append(t, "ev")
	for range maxAttempts {
		f.runOnce(t, true)
		f.clock.advance(retryDelay)
	}
	if got := f.get(t, "ev"); got.Status != store.EventDead || got.Attempts != maxAttempts || got.LastError != "always" {
		t.Fatalf("after %d failures = %+v; want dead", maxAttempts, got)
	}
	f.runOnce(t, false)
	if calls != maxAttempts {
		t.Fatalf("handler ran %d times; want %d", calls, maxAttempts)
	}
	log := f.log.String()
	if strings.Count(log, "level=WARN msg=\"handler failed\"") != maxAttempts ||
		strings.Count(log, "level=ERROR msg=\"dead-lettering") != 1 || !strings.Contains(log, "retry_at=") {
		t.Fatalf("want a warning per failure and one dead-letter error, got: %s", log)
	}
}

func TestRunOnceDeadLettersClaimPastMaxWithoutHandling(t *testing.T) {
	f := newFixture(t, func(context.Context, store.Event) error {
		t.Error("handler ran for an event past max attempts")
		return nil
	}, func(c *Config) { c.MaxAttempts = 2 })
	f.append(t, "ev")
	for i := range 2 { // MaxAttempts workers crash, each claim expiring unreleased
		if _, err := f.store.ClaimEvent(f.ctx, t0.Add(time.Duration(i)*lease), lease); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	f.clock.advance(2 * lease) // the second lease expires exactly now
	f.runOnce(t, true)

	// The worker's own claim is attempt MaxAttempts+1, the first one over the limit.
	got := f.get(t, "ev")
	if got.Status != store.EventDead || got.Attempts != 3 || !strings.Contains(got.LastError, "exceeds") {
		t.Fatalf("event = %+v; want dead at attempt 3 with an explanatory error", got)
	}
	if !strings.Contains(f.log.String(), "level=ERROR") {
		t.Fatalf("dead-lettering was not logged at error level: %s", f.log)
	}
}

// When Complete or Fail itself fails, RunOnce reports it; it is not a lost lease.
func TestRunOnceReturnsRecordingErrors(t *testing.T) {
	for name, handlerErr := range map[string]error{"complete": nil, "fail": errors.New("boom")} {
		t.Run(name, func(t *testing.T) {
			var f *fixture
			f = newFixture(t, func(context.Context, store.Event) error {
				_ = f.store.Close()
				return handlerErr
			})
			f.append(t, "ev")
			handled, err := f.w.RunOnce(f.ctx)
			if !handled || err == nil || errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "record outcome") {
				t.Fatalf("RunOnce = %v, %v; want handled and a non-conflict \"record outcome\" error", handled, err)
			}
		})
	}
}

// A handler that outlives its lease sees the event reclaimed; the fenced
// Complete or Fail then conflicts, which must not be an error or change B's lease.
func TestRunOnceLostLeaseIsNotAnError(t *testing.T) {
	for name, handlerErr := range map[string]error{"complete": nil, "fail": errors.New("late")} {
		t.Run(name, func(t *testing.T) {
			var f *fixture
			f = newFixture(t, func(ctx context.Context, _ store.Event) error {
				b, err := f.store.ClaimEvent(ctx, t0.Add(lease), lease)
				if err != nil || b.Attempts != 2 {
					t.Errorf("reclaim = %+v, %v; want attempt 2", b, err)
				}
				return handlerErr
			})
			f.append(t, "ev")
			f.runOnce(t, true)

			got := f.get(t, "ev")
			if got.Status != store.EventProcessing || got.Attempts != 2 || got.LastError != "" ||
				!strings.Contains(f.log.String(), "lease lost") {
				t.Fatalf("event = %+v, log %q; want B's lease untouched and a lease-lost log", got, f.log)
			}
		})
	}
}

func TestRunOnceRecoversHandlerPanic(t *testing.T) {
	f := newFixture(t, func(context.Context, store.Event) error { panic("kaboom") })
	f.append(t, "ev")
	f.runOnce(t, true)

	got := f.get(t, "ev")
	if got.Status != store.EventPending || got.LastError != "panic: kaboom" || !got.NextAttemptAt.Equal(t0.Add(retryDelay)) {
		t.Fatalf("event = %+v; want pending at %v with last error %q", got, t0.Add(retryDelay), "panic: kaboom")
	}
	if log := f.log.String(); !strings.Contains(log, "kaboom") || !strings.Contains(log, "TestRunOnceRecoversHandlerPanic") {
		t.Fatalf("log lacks the panic value and stack: %s", log)
	}
}

func TestRunOnceReturnsStoreErrors(t *testing.T) {
	f := newFixture(t, func(context.Context, store.Event) error { return nil })
	_ = f.store.Close()
	if handled, err := f.w.RunOnce(f.ctx); err == nil || handled || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RunOnce on closed store = %v, %v; want a non-NotFound error", handled, err)
	}
}

func TestNewValidatesAndDefaults(t *testing.T) {
	s, h := &sqlite.Store{}, HandlerFunc(func(context.Context, store.Event) error { return nil })
	valid := Config{Queue: s, Handler: h, Lease: lease, MaxAttempts: 1, Backoff: func(int) time.Duration { return 0 }}
	w, err := New(valid)
	if err != nil {
		t.Fatalf("New(valid, defaults unset) = %v", err)
	}
	if w.cfg.Now == nil || w.cfg.Logger == nil {
		t.Fatalf("New left defaults unset: Now nil = %v, Logger nil = %v", w.cfg.Now == nil, w.cfg.Logger == nil)
	}
	for name, mod := range map[string]func(*Config){
		"nil queue":    func(c *Config) { c.Queue = nil },
		"nil handler":  func(c *Config) { c.Handler = nil },
		"nil backoff":  func(c *Config) { c.Backoff = nil },
		"zero lease":   func(c *Config) { c.Lease = 0 },
		"zero max":     func(c *Config) { c.MaxAttempts = 0 },
		"neg lease":    func(c *Config) { c.Lease = -time.Second },
		"neg attempts": func(c *Config) { c.MaxAttempts = -1 },
	} {
		cfg := valid
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%s) succeeded; want an error", name)
		}
	}
}
