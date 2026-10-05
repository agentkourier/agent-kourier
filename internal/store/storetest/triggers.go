package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	tBinding = "sre/sre-alerts"
	tKey     = "7d1c0f3a9b" // a key hash; the store reads it as an opaque string
	tCool    = 4 * time.Hour
	rConn    = "courier/slack-main"
	rChannel = "C0ALERTS"
)

// runLimits is the preset's limits with a 10 minute window: five starts in it, two at a time, 40 a day.
var runLimits = store.TriggerRunLimits{MaxRuns: 5, Per: 10 * time.Minute, MaxConcurrent: 2, MaxPerDay: 40, DigestWindow: 10 * time.Minute}

func opening(ts string, at time.Time) store.TriggerThread {
	return store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: ts, Opened: at}
}

func reserve(t *testing.T, s store.Store, th store.TriggerThread) (store.TriggerThread, bool, error) {
	t.Helper()
	return s.OpenTriggerThread(t.Context(), th, store.TriggerStale{})
}

func startAt(t *testing.T, s store.Store, ts string, at time.Time) {
	t.Helper()
	if _, opened, err := reserve(t, s, opening(ts, at)); err != nil || !opened {
		t.Fatalf("OpenTriggerThread(%s) = %v, %v; want it reserved", ts, opened, err)
	}
	if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, ts, at.Add(tCool)); err != nil {
		t.Fatal(err)
	}
}

func closeAt(t *testing.T, s store.Store, at time.Time) {
	t.Helper()
	if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, at); err != nil || !ok {
		t.Fatalf("CloseTriggerThread = %v, %v; want it closed", ok, err)
	}
}

func (su suite) triggerThreads(t *testing.T) {
	su.run(t, "a key with no row reserves a thread, which starts open and not started", func(t *testing.T, s store.Store) {
		got, opened, err := reserve(t, s, opening("100.1", t0))
		if err != nil || !opened {
			t.Fatalf("OpenTriggerThread = %+v, %v, %v; want it opened", got, opened, err)
		}
		want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "100.1", Opened: t0, LastSeen: t0}
		if !reflect.DeepEqual(got, want) || got.Started() {
			t.Errorf("thread = %+v; want %+v, not started", got, want)
		}
		if again, err := s.GetTriggerThread(t.Context(), tBinding, tKey); err != nil || !reflect.DeepEqual(again, want) {
			t.Errorf("GetTriggerThread = %+v, %v; want %+v", again, err, want)
		}
	})

	su.run(t, "a reservation ignores the fields the row owns", func(t *testing.T, s store.Store) {
		in := opening("100.1", t0)
		in.LastSeen, in.ClosedAt, in.CooldownUntil = t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(tCool)
		got, _, err := reserve(t, s, in)
		if err != nil || !got.LastSeen.Equal(t0) || !got.ClosedAt.IsZero() || got.Started() {
			t.Errorf("OpenTriggerThread = %+v, %v; want last seen %v, not closed, not started", got, err, t0)
		}
	})

	su.run(t, "marking a thread started starts its cooldown, once", func(t *testing.T, s store.Store) {
		if _, _, err := reserve(t, s, opening("100.1", t0)); err != nil {
			t.Fatal(err)
		}
		until := t0.Add(tCool)
		for range 2 { // the same again is the same
			if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", until); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := s.GetTriggerThread(t.Context(), tBinding, tKey)
		if !got.CooldownUntil.Equal(until) || !got.Started() || got.Thread != "100.1" || !got.ClosedAt.IsZero() {
			t.Errorf("row = %+v; want it started with the cooldown until %v and nothing else changed", got, until)
		}
	})

	su.run(t, "marking refuses a key with no row, another message's thread, and incomplete arguments", func(t *testing.T, s store.Store) {
		if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", t0.Add(tCool)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a key with no row: %v; want ErrNotFound", err)
		}
		if _, _, err := reserve(t, s, opening("100.1", t0)); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "200.2", t0.Add(tCool)); !errors.Is(err, store.ErrConflict) {
			t.Errorf("another message's thread: %v; want ErrConflict", err)
		}
		if got, _ := s.GetTriggerThread(t.Context(), tBinding, tKey); got.Started() {
			t.Errorf("the refused mark started the thread: %+v", got)
		}
		for name, args := range map[string][3]string{"no binding": {"", tKey, "100.1"}, "no key hash": {tBinding, "", "100.1"}, "no thread": {tBinding, tKey, ""}} {
			if err := s.MarkTriggerStarted(t.Context(), args[0], args[1], args[2], t0.Add(tCool)); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v; want ErrInvalid", name, err)
			}
		}
		for name, at := range map[string]time.Time{"no time": {}, "the epoch": time.Unix(0, 0)} {
			if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", at); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v; want ErrInvalid", name, err)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		closed bool
		at     time.Time
	}{
		{"open, inside the cooldown", false, t0.Add(time.Hour)},
		{"open, past the cooldown", false, t0.Add(tCool + time.Hour)},
		{"closed, inside the cooldown", true, t0.Add(time.Hour)},
		{"closed, one tick before the cooldown ends", true, t0.Add(tCool - time.Microsecond)},
	} {
		su.run(t, "a key with a started thread gets that thread back: "+tc.name, func(t *testing.T, s store.Store) {
			startAt(t, s, "100.1", t0)
			if tc.closed {
				closeAt(t, s, t0.Add(time.Minute))
			}
			got, opened, err := reserve(t, s, opening("200.2", tc.at))
			if err != nil || opened {
				t.Fatalf("OpenTriggerThread = %+v, %v, %v; want the existing thread and no reservation", got, opened, err)
			}
			if got.Thread != "100.1" || !got.Opened.Equal(t0) || got.ClosedAt.IsZero() == tc.closed || !got.Started() {
				t.Errorf("row = %+v; want the first thread, untouched (closed: %v)", got, tc.closed)
			}
		})
	}

	for name, at := range map[string]time.Time{"at the end of the cooldown": t0.Add(tCool), "after it": t0.Add(tCool + time.Hour)} {
		su.run(t, "a closed thread past its cooldown is replaced: "+name, func(t *testing.T, s store.Store) {
			startAt(t, s, "100.1", t0)
			closeAt(t, s, t0.Add(time.Minute))
			got, opened, err := reserve(t, s, opening("200.2", at))
			if err != nil || !opened {
				t.Fatalf("OpenTriggerThread = %+v, %v, %v; want a new thread", got, opened, err)
			}
			want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "200.2", Opened: at, LastSeen: at}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("thread = %+v; want %+v", got, want)
			}
		})
	}

	su.run(t, "a reservation whose turn never started is taken over once it is stale", func(t *testing.T, s store.Store) {
		const grace = time.Minute
		if _, _, err := reserve(t, s, opening("100.1", t0)); err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			ts string
			at time.Time
		}{
			"the same message again, long after":            {"100.1", t0.Add(time.Hour)},
			"another message, inside the grace":             {"200.2", t0.Add(grace / 2)},
			"another message, at the end of the grace":      {"200.2", t0.Add(grace)},
			"another message, a tick before the grace ends": {"200.2", t0.Add(grace - time.Microsecond)},
		} {
			got, opened, err := s.OpenTriggerThread(t.Context(), opening(tc.ts, tc.at), store.TriggerStale{Unstarted: tc.at.Add(-grace)})
			if err != nil || opened || got.Thread != "100.1" || !got.Opened.Equal(t0) {
				t.Errorf("%s: %+v, opened %v, %v; want the first reservation back", name, got, opened, err)
			}
		}
		at := t0.Add(grace + time.Microsecond)
		got, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", at), store.TriggerStale{Unstarted: at.Add(-grace)})
		if err != nil || !opened || got.Thread != "200.2" || !got.Opened.Equal(at) {
			t.Fatalf("another message, past the grace: %+v, opened %v, %v; want the reservation taken over", got, opened, err)
		}
		if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", t0.Add(tCool)); !errors.Is(err, store.ErrConflict) {
			t.Errorf("the late mark of the first message: %v; want ErrConflict", err)
		}
		if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "200.2", at.Add(tCool)); err != nil {
			t.Fatal(err)
		}
		if _, opened, err := s.OpenTriggerThread(t.Context(), opening("300.3", at.Add(24*time.Hour)), store.TriggerStale{Unstarted: at.Add(23 * time.Hour)}); err != nil || opened {
			t.Errorf("a started thread, a day on, an open one: opened %v, %v; want it kept", opened, err)
		}
	})

	su.run(t, "the second delivery of the message that reserved a thread finds its own row", func(t *testing.T, s store.Store) {
		for i := range 2 {
			got, opened, err := reserve(t, s, opening("100.1", t0))
			if err != nil || opened != (i == 0) || got.Thread != "100.1" {
				t.Fatalf("delivery %d: %+v, opened %v, %v", i, got, opened, err)
			}
		}
	})

	su.run(t, "of many starts of one key at once exactly one reserves the thread", func(t *testing.T, s store.Store) {
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ts := fmt.Sprint("100.", i)
				if _, opened, err := s.OpenTriggerThread(t.Context(), opening(ts, t0), store.TriggerStale{Unstarted: t0.Add(-time.Minute), Idle: t0.Add(-time.Minute)}); err != nil {
					t.Error(err)
				} else if opened {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Errorf("%d starts won; want 1", wins.Load())
		}
	})

	su.run(t, "threads are told apart by Binding and key", func(t *testing.T, s store.Store) {
		for i, k := range []struct{ binding, key string }{{tBinding, "k1"}, {tBinding, "k2"}, {"payments/pay", "k1"}} {
			in := opening("100."+string(rune('1'+i)), t0)
			in.Binding, in.KeyHash = k.binding, k.key
			if _, opened, err := reserve(t, s, in); err != nil || !opened {
				t.Fatalf("%+v: opened %v, %v; want each its own thread", k, opened, err)
			}
		}
		if err := s.ReopenTriggerThread(t.Context(), tBinding, "k1", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		other, _ := s.GetTriggerThread(t.Context(), "payments/pay", "k1")
		if !other.LastSeen.Equal(t0) {
			t.Errorf("a repeat of one Binding's key moved another's last seen to %v", other.LastSeen)
		}
	})

	su.run(t, "a thread is closed once and keeps its first close time", func(t *testing.T, s store.Store) {
		startAt(t, s, "100.1", t0)
		first := t0.Add(10 * time.Minute)
		closeAt(t, s, first)
		if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, first.Add(time.Hour)); err != nil || ok {
			t.Errorf("second close = %v, %v; want false, nil", ok, err)
		}
		got, err := s.GetTriggerThread(t.Context(), tBinding, tKey)
		if err != nil || !got.ClosedAt.Equal(first) || !got.LastSeen.Equal(first) || got.Thread != "100.1" {
			t.Errorf("row = %+v, %v; want closed at %v, last seen then, same thread", got, err, first)
		}
	})

	su.run(t, "a repeat of the key reopens its thread and moves last seen forward only", func(t *testing.T, s store.Store) {
		startAt(t, s, "100.1", t0)
		closeAt(t, s, t0.Add(10*time.Minute))
		later := t0.Add(time.Hour)
		if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, later); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTriggerThread(t.Context(), tBinding, tKey)
		if !got.ClosedAt.IsZero() || !got.LastSeen.Equal(later) || !got.CooldownUntil.Equal(t0.Add(tCool)) {
			t.Errorf("row = %+v; want it open, last seen %v, and the cooldown unchanged", got, later)
		}
		if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, t0); err != nil {
			t.Fatal(err)
		}
		if got, _ = s.GetTriggerThread(t.Context(), tBinding, tKey); !got.LastSeen.Equal(later) {
			t.Errorf("last seen = %v after an older repeat; want %v", got.LastSeen, later)
		}
		if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, later.Add(time.Minute)); err != nil || !ok {
			t.Errorf("close of the reopened thread = %v, %v; want it closed", ok, err)
		}
	})

	su.run(t, "operations on a key with no row are not found", func(t *testing.T, s store.Store) {
		if _, err := s.GetTriggerThread(t.Context(), tBinding, tKey); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetTriggerThread: %v; want ErrNotFound", err)
		}
		if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, t0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ReopenTriggerThread: %v; want ErrNotFound", err)
		}
		if _, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, t0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("CloseTriggerThread: %v; want ErrNotFound", err)
		}
	})

	su.run(t, "operations refuse incomplete arguments", func(t *testing.T, s store.Store) {
		for name, mutate := range map[string]func(*store.TriggerThread){
			"no binding": func(t *store.TriggerThread) { t.Binding = "" }, "no key hash": func(t *store.TriggerThread) { t.KeyHash = "" },
			"no thread": func(t *store.TriggerThread) { t.Thread = "" }, "no time": func(t *store.TriggerThread) { t.Opened = time.Time{} },
			"the epoch": func(t *store.TriggerThread) { t.Opened = time.Unix(0, 0) },
		} {
			in := opening("100.1", t0)
			mutate(&in)
			if _, _, err := reserve(t, s, in); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: err = %v; want ErrInvalid", name, err)
			}
		}
		if err := s.ReopenTriggerThread(t.Context(), "", tKey, t0); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("reopen with no binding: %v; want ErrInvalid", err)
		}
		if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, time.Time{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("reopen with no time: %v; want ErrInvalid", err)
		}
		if _, err := s.CloseTriggerThread(t.Context(), tBinding, "", t0); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("close with no key hash: %v; want ErrInvalid", err)
		}
		if _, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, time.Time{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("close with no time: %v; want ErrInvalid", err)
		}
		if _, err := s.GetTriggerThread(t.Context(), "", tKey); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("get with no binding: %v; want ErrInvalid", err)
		}
	})

	su.runReopen(t, "a thread, its close and its cooldown survive a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		startAt(t, s, "100.1", t0)
		closeAt(t, s, t0.Add(10*time.Minute))
		s = reopen()
		got, err := s.GetTriggerThread(t.Context(), tBinding, tKey)
		want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "100.1", Opened: t0, LastSeen: t0.Add(10 * time.Minute), ClosedAt: t0.Add(10 * time.Minute), CooldownUntil: t0.Add(tCool)}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("after reopening: %+v, %v; want %+v", got, err, want)
		}
		if _, opened, err := reserve(t, s, opening("200.2", t0.Add(time.Hour))); err != nil || opened {
			t.Errorf("a start inside the cooldown after the restart: opened %v, %v; want the cooldown kept", opened, err)
		}
	})

	su.run(t, "an open thread that went idle is taken over", func(t *testing.T, s store.Store) {
		const maxIdle = 24 * time.Hour
		startAt(t, s, "100.1", t0)
		try := func(ts string, at time.Time) (store.TriggerThread, bool) {
			t.Helper()
			got, opened, err := s.OpenTriggerThread(t.Context(), opening(ts, at), store.TriggerStale{Idle: at.Add(-maxIdle)})
			if err != nil {
				t.Fatal(err)
			}
			return got, opened
		}
		if got, opened := try("200.2", t0.Add(maxIdle)); opened || got.Thread != "100.1" {
			t.Errorf("idle for exactly maxIdle: %+v, opened %v; want the thread kept", got, opened)
		}
		if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, t0.Add(20*time.Hour)); err != nil { // a repeat
			t.Fatal(err)
		}
		if got, opened := try("300.3", t0.Add(maxIdle+time.Hour)); opened || got.Thread != "100.1" {
			t.Errorf("seen 5 h ago: %+v, opened %v; want the thread kept", got, opened)
		}
		at := t0.Add(20*time.Hour + maxIdle + time.Microsecond)
		got, opened := try("400.4", at)
		if !opened || got.Thread != "400.4" || !got.Opened.Equal(at) || got.Started() || !got.ClosedAt.IsZero() {
			t.Errorf("idle past maxIdle: %+v, opened %v; want a new reservation", got, opened)
		}
		if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", at.Add(tCool)); !errors.Is(err, store.ErrConflict) {
			t.Errorf("the abandoned thread's late mark: %v; want ErrConflict", err)
		}
	})

	su.run(t, "with no idle cutoff an open thread is never taken over", func(t *testing.T, s store.Store) {
		startAt(t, s, "100.1", t0)
		if got, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", t0.Add(1000*time.Hour)), store.TriggerStale{}); err != nil || opened || got.Thread != "100.1" {
			t.Errorf("OpenTriggerThread = %+v, %v, %v; want the thread kept", got, opened, err)
		}
	})

	su.run(t, "the purge drops closed threads past their cooldown and open ones idle past the cutoff", func(t *testing.T, s store.Store) {
		put := func(key string, at time.Time, closed bool) {
			t.Helper()
			in := opening("1."+key, at)
			in.KeyHash = key
			if _, opened, err := reserve(t, s, in); err != nil || !opened {
				t.Fatalf("%s: %v, %v", key, opened, err)
			}
			if err := s.MarkTriggerStarted(t.Context(), tBinding, key, in.Thread, at.Add(tCool)); err != nil {
				t.Fatal(err)
			}
			if closed {
				if _, err := s.CloseTriggerThread(t.Context(), tBinding, key, at.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
		}
		now := t0.Add(48 * time.Hour)
		put("closed-past-cooldown", t0, true)
		put("closed-in-cooldown", now.Add(-time.Hour), true)
		put("closed-at-cooldown-end", now.Add(-tCool), true) // its cooldown ends at now, not before it
		put("open-idle", t0, false)
		put("open-recent", now.Add(-time.Hour), false)
		if _, _, err := reserve(t, s, opening("2.1", t0)); err != nil { // a reservation that never started, long ago
			t.Fatal(err)
		}

		n, err := s.PurgeTriggerThreads(t.Context(), now, time.Time{})
		if err != nil || n != 1 {
			t.Fatalf("PurgeTriggerThreads with no idle cutoff = %d, %v; want only the closed one past its cooldown", n, err)
		}
		n, err = s.PurgeTriggerThreads(t.Context(), now, now.Add(-24*time.Hour))
		if err != nil || n != 2 {
			t.Fatalf("PurgeTriggerThreads = %d, %v; want the idle open thread and the old reservation", n, err)
		}
		for key, want := range map[string]bool{"closed-past-cooldown": false, "open-idle": false, "closed-in-cooldown": true, "open-recent": true, "closed-at-cooldown-end": true} {
			if _, err := s.GetTriggerThread(t.Context(), tBinding, key); (err == nil) != want {
				t.Errorf("%s kept: %v, want %v", key, err == nil, want)
			}
		}
		if n, err := s.PurgeTriggerThreads(t.Context(), now.Add(time.Microsecond), time.Time{}); err != nil || n != 1 {
			t.Errorf("a purge a microsecond later = %d, %v; want the thread whose cooldown has now ended", n, err)
		}
		if _, err := s.PurgeTriggerThreads(t.Context(), time.Time{}, time.Time{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("a purge with no time: %v; want ErrInvalid", err)
		}
	})

	su.run(t, "a reservation that a limit stopped is released", func(t *testing.T, s store.Store) {
		if _, opened, err := reserve(t, s, opening("100.1", t0)); err != nil || !opened {
			t.Fatalf("OpenTriggerThread = %v, %v", opened, err)
		}
		if rel, err := s.ReleaseTriggerThread(t.Context(), tBinding, tKey, "100.1"); err != nil || !rel {
			t.Fatalf("ReleaseTriggerThread = %v, %v; want it released", rel, err)
		}
		if _, err := s.GetTriggerThread(t.Context(), tBinding, tKey); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetTriggerThread after the release = %v; want ErrNotFound", err)
		}
		if got, opened, err := reserve(t, s, opening("100.2", t0.Add(time.Second))); err != nil || !opened || got.Thread != "100.2" {
			t.Errorf("the next fire = %+v, %v, %v; want it reserved at once", got, opened, err)
		}
		if rel, err := s.ReleaseTriggerThread(t.Context(), tBinding, tKey, "100.1"); err != nil || rel {
			t.Errorf("ReleaseTriggerThread of the message that no longer holds the key = %v, %v; want the other's row left", rel, err)
		}
	})

	su.run(t, "a started thread is not released", func(t *testing.T, s store.Store) {
		startAt(t, s, "100.1", t0)
		if rel, err := s.ReleaseTriggerThread(t.Context(), tBinding, tKey, "100.1"); err != nil || rel {
			t.Fatalf("ReleaseTriggerThread of a started thread = %v, %v; want it left", rel, err)
		}
		if th, err := s.GetTriggerThread(t.Context(), tBinding, tKey); err != nil || !th.Started() {
			t.Errorf("thread = %+v, %v; want it started still", th, err)
		}
		if rel, err := s.ReleaseTriggerThread(t.Context(), tBinding, "nokey", "100.1"); err != nil || rel {
			t.Errorf("ReleaseTriggerThread of a missing key = %v, %v; want nothing", rel, err)
		}
		if _, err := s.ReleaseTriggerThread(t.Context(), tBinding, tKey, ""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("ReleaseTriggerThread with no thread = %v; want ErrInvalid", err)
		}
	})

}

func runOf(ts string, at time.Time) store.TriggerRun {
	return store.TriggerRun{Binding: tBinding, Trigger: 1, Key: store.ConversationKey{Connection: rConn, Channel: rChannel, Thread: ts}, At: at}
}

func admit(t *testing.T, s store.Store, run store.TriggerRun, lim store.TriggerRunLimits) store.TriggerAdmission {
	t.Helper()
	got, err := s.AdmitTriggerRun(t.Context(), run, lim)
	if err != nil {
		t.Fatalf("AdmitTriggerRun(%s) = %v", run.Key.Thread, err)
	}
	return got
}

func running(t *testing.T, s store.Store, ts string, at time.Time) {
	t.Helper()
	key := runOf(ts, at).Key
	if err := s.UpsertSession(t.Context(), store.Session{Key: key, Binding: tBinding, TaskID: "task-" + ts, LastActivity: at}); err != nil {
		t.Fatal(err)
	}
}

func (su suite) triggerRuns(t *testing.T) {
	su.run(t, "a start under the limits is admitted", func(t *testing.T, s store.Store) {
		if got := admit(t, s, runOf("100.1", t0), runLimits); !got.Admitted || got.Limit != "" {
			t.Errorf("the first start = %+v; want admitted", got)
		}
	})

	su.run(t, "the rate limit counts the starts in a sliding window", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 3, Per: 10 * time.Minute, DigestWindow: 10 * time.Minute}
		for i := range 3 {
			if got := admit(t, s, runOf(fmt.Sprint("1.", i), t0.Add(time.Duration(i)*time.Minute)), lim); !got.Admitted {
				t.Fatalf("start %d = %+v; want admitted", i, got)
			}
		}
		for _, tt := range []struct {
			name string
			at   time.Duration // after t0
			want store.TriggerAdmission
		}{
			{"the fourth within the window", 3 * time.Minute, store.TriggerAdmission{Limit: store.LimitRate}},
			{"just before the first leaves the window", 10*time.Minute - time.Microsecond, store.TriggerAdmission{Limit: store.LimitRate}},
			{"when the first is exactly a window old it no longer counts", 10 * time.Minute, store.TriggerAdmission{Admitted: true}},
		} {
			if got := admit(t, s, runOf("2."+tt.name, t0.Add(tt.at)), lim); got != tt.want {
				t.Errorf("%s: %+v; want %+v", tt.name, got, tt.want)
			}
		}
	})

	su.run(t, "the daily limit counts the starts of the last 24 hours", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxPerDay: 2, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		admit(t, s, runOf("1.2", t0.Add(12*time.Hour)), lim)
		if got := admit(t, s, runOf("1.3", t0.Add(23*time.Hour)), lim); got.Admitted || got.Limit != store.LimitDaily {
			t.Errorf("the third in a day = %+v; want daily", got)
		}
		if got := admit(t, s, runOf("1.4", t0.Add(24*time.Hour)), lim); !got.Admitted {
			t.Errorf("a start when the first is a day old = %+v; want admitted", got)
		}
	})

	su.run(t, "the concurrency limit counts the investigations that are running", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxConcurrent: 2, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		admit(t, s, runOf("1.2", t0.Add(time.Second)), lim)
		running(t, s, "1.1", t0)
		running(t, s, "1.2", t0)

		if got := admit(t, s, runOf("1.3", t0.Add(2*time.Second)), lim); got.Admitted || got.Limit != store.LimitConcurrent {
			t.Fatalf("a third with two running = %+v; want concurrent", got)
		}
		// One ends: the session's task is cleared, as the manager does when the task ends.
		if err := s.SetSessionTask(t.Context(), runOf("1.1", t0).Key, "", "", store.TaskTurn{}); err != nil {
			t.Fatal(err)
		}
		if got := admit(t, s, runOf("1.3", t0.Add(3*time.Second)), lim); !got.Admitted {
			t.Errorf("a start when one of the two has ended = %+v; want admitted", got)
		}
	})

	su.run(t, "a queued turn that has not begun counts as running", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxConcurrent: 1, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		key := runOf("1.1", t0).Key
		if err := s.UpsertSession(t.Context(), store.Session{Key: key, Binding: tBinding, LastActivity: t0}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnqueueReply(t.Context(), store.Reply{ID: "turn-1.1", Key: key, UserID: "U1", MessageRef: "1.1", Text: "prompt", EnqueuedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted || got.Limit != store.LimitConcurrent {
			t.Fatalf("a start with the first turn queued = %+v; want concurrent", got)
		}
		// The turn is delivered and its task ends: nothing is running.
		if err := s.MarkReplyDelivered(t.Context(), "turn-1.1", t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if got := admit(t, s, runOf("1.2", t0.Add(2*time.Second)), lim); !got.Admitted {
			t.Errorf("a start once the first turn is delivered and its task is over = %+v; want admitted", got)
		}
	})

	su.run(t, "a session counts once however many replies it has", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxConcurrent: 2, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		running(t, s, "1.1", t0)
		key := runOf("1.1", t0).Key
		for i := range 2 {
			if _, err := s.EnqueueReply(t.Context(), store.Reply{ID: fmt.Sprint("reply-", i), Key: key, UserID: "U1", MessageRef: "1.1", Text: "and?", EnqueuedAt: t0}); err != nil {
				t.Fatal(err)
			}
		}
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); !got.Admitted {
			t.Errorf("a second start with one investigation (a task and two replies) = %+v; want admitted", got)
		}
	})

	su.run(t, "when several limits are reached the longest lasting is reported", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, MaxConcurrent: 1, MaxPerDay: 1, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		running(t, s, "1.1", t0)
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Limit != store.LimitDaily {
			t.Errorf("all three reached: %+v; want daily", got)
		}
		lim.MaxPerDay = 0
		if got := admit(t, s, runOf("1.3", t0.Add(time.Second)), lim); got.Limit != store.LimitRate {
			t.Errorf("rate and concurrent reached: %+v; want rate", got)
		}
		lim.MaxRuns = 0
		if got := admit(t, s, runOf("1.4", t0.Add(time.Second)), lim); got.Limit != store.LimitConcurrent {
			t.Errorf("only concurrent reached: %+v; want concurrent", got)
		}
		if got := admit(t, s, runOf("1.5", t0.Add(time.Second)), store.TriggerRunLimits{DigestWindow: time.Hour}); !got.Admitted {
			t.Errorf("no limits: %+v; want admitted", got)
		}
	})

	su.run(t, "each Binding and trigger is counted on its own", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		other := runOf("1.2", t0)
		other.Trigger = 2
		if got := admit(t, s, other, lim); !got.Admitted {
			t.Errorf("another trigger of the Binding = %+v; want admitted", got)
		}
		other = runOf("1.3", t0)
		other.Binding = "sre/other"
		if got := admit(t, s, other, lim); !got.Admitted {
			t.Errorf("another Binding's trigger = %+v; want admitted", got)
		}
		if got := admit(t, s, runOf("1.4", t0), lim); got.Admitted {
			t.Errorf("the same trigger again = %+v; want rate", got)
		}
	})

	su.run(t, "asking for a message again is the same start", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		for range 3 {
			if got := admit(t, s, runOf("1.1", t0), lim); !got.Admitted {
				t.Fatalf("the same message again = %+v; want admitted", got)
			}
		}
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted {
			t.Errorf("another message after one was asked three times = %+v; want rate", got)
		}
	})

	su.run(t, "a stopped message is checked afresh when asked for again", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Minute, DigestWindow: time.Minute}
		admit(t, s, runOf("1.1", t0), lim)
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted {
			t.Fatalf("the second = %+v; want stopped", got)
		}
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted || got.Limit != store.LimitRate {
			t.Errorf("the same stopped message again = %+v; want stopped, rate", got)
		}
		if got := admit(t, s, runOf("1.2", t0.Add(2*time.Minute)), lim); !got.Admitted {
			t.Fatalf("the stopped message once the window has passed = %+v; want admitted", got)
		}
		ds, err := s.ListTriggerDigests(t.Context(), t0.Add(time.Hour))
		if err != nil || len(ds) != 0 {
			t.Errorf("digests = %+v, %v; want none, since the message started after all", ds, err)
		}
	})

	// Two starts must not both pass the last free place: the check of the limits and the record of the start are
	// one step.
	su.run(t, "of many starts at once only as many as the limit allows are admitted", func(t *testing.T, s store.Store) {
		const starts, allowed = 30, 5
		lim := store.TriggerRunLimits{MaxRuns: allowed, Per: time.Hour, DigestWindow: 10 * time.Minute}
		var admitted atomic.Int32
		parallel(starts, func(i int) {
			got, err := s.AdmitTriggerRun(t.Context(), runOf(fmt.Sprint("1.", i), t0.Add(time.Duration(i)*time.Millisecond)), lim)
			switch {
			case err != nil:
				t.Errorf("AdmitTriggerRun: %v", err)
			case got.Admitted:
				admitted.Add(1)
			case got.Limit != store.LimitRate:
				t.Errorf("a stopped start = %+v; want the rate limit", got)
			}
		})
		if admitted.Load() != allowed {
			t.Errorf("%d starts admitted; want exactly %d", admitted.Load(), allowed)
		}
		ds, err := s.ListTriggerDigests(t.Context(), t0.Add(2*time.Hour))
		if err != nil || len(ds) != 1 || ds[0].Counts[store.LimitRate] != starts-allowed {
			t.Errorf("digests = %+v, %v; want one window counting the %d stopped starts", ds, err, starts-allowed)
		}
	})

	su.run(t, "of many asks for one message at once all are admitted and it counts once", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		parallel(16, func(int) {
			if got, err := s.AdmitTriggerRun(t.Context(), runOf("1.1", t0), lim); err != nil || !got.Admitted {
				t.Errorf("the same message = %+v, %v; want admitted", got, err)
			}
		})
		if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted || got.Limit != store.LimitRate {
			t.Errorf("another message after the one was asked 16 times = %+v; want the rate limit", got)
		}
	})

	su.runReopen(t, "the counts and the stopped starts survive a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 2, Per: time.Hour, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		admit(t, s, runOf("1.2", t0), lim)
		if got := admit(t, s, runOf("1.3", t0), lim); got.Admitted {
			t.Fatal("the third was admitted")
		}
		s = reopen()
		if got := admit(t, s, runOf("1.4", t0.Add(time.Minute)), lim); got.Admitted || got.Limit != store.LimitRate {
			t.Errorf("a start after a reopen = %+v; want rate", got)
		}
		if ds, err := s.ListTriggerDigests(t.Context(), t0.Add(2*time.Hour)); err != nil || len(ds) != 1 || ds[0].Counts[store.LimitRate] != 2 {
			t.Errorf("digests after a reopen = %+v, %v; want the two stopped starts", ds, err)
		}
	})

	su.run(t, "a digest window opens at its first stopped start", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: 10 * time.Minute}
		admit(t, s, runOf("1.0", t0), lim)
		// A burst that begins a while after the start: its window opens with its first stopped start, on no clock's boundary.
		first := t0.Add(7 * time.Minute)
		for i, d := range []time.Duration{0, time.Second, 9 * time.Minute} {
			admit(t, s, runOf(fmt.Sprint("2.", i), first.Add(d)), lim)
		}
		later := first.Add(10*time.Minute + time.Second) // after the window that opened at first
		admit(t, s, runOf("3.0", later), lim)

		if ds, err := s.ListTriggerDigests(t.Context(), first.Add(10*time.Minute-time.Microsecond)); err != nil || len(ds) != 0 {
			t.Fatalf("digests before the window ended = %+v, %v; want none", ds, err)
		}
		ds, err := s.ListTriggerDigests(t.Context(), later.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := []store.TriggerDigest{
			{Binding: tBinding, Trigger: 1, Connection: rConn, Channel: rChannel, Start: first, End: first.Add(10 * time.Minute), Counts: map[store.TriggerLimit]int{store.LimitRate: 3}},
			{Binding: tBinding, Trigger: 1, Connection: rConn, Channel: rChannel, Start: later, End: later.Add(10 * time.Minute), Counts: map[store.TriggerLimit]int{store.LimitRate: 1}},
		}
		if !reflect.DeepEqual(ds, want) {
			t.Fatalf("digests = %+v\nwant     %+v", ds, want)
		}

		if err := s.MarkTriggerDigested(t.Context(), tBinding, 1, first); err != nil {
			t.Fatal(err)
		}
		ds, err = s.ListTriggerDigests(t.Context(), later.Add(time.Hour))
		if err != nil || len(ds) != 1 || !ds[0].Start.Equal(later) {
			t.Errorf("digests after the first was marked = %+v, %v; want only the later window", ds, err)
		}
		if err := s.MarkTriggerDigested(t.Context(), tBinding, 1, first); err != nil {
			t.Errorf("marking a window that is marked already = %v; want nothing", err)
		}
	})

	su.run(t, "a digest counts the stopped starts by limit", func(t *testing.T, s store.Store) {
		admit(t, s, runOf("1.0", t0), store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour})
		rate := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		daily := store.TriggerRunLimits{MaxPerDay: 1, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0.Add(time.Second)), rate)
		admit(t, s, runOf("1.2", t0.Add(2*time.Second)), rate)
		admit(t, s, runOf("1.3", t0.Add(3*time.Second)), daily)
		ds, err := s.ListTriggerDigests(t.Context(), t0.Add(2*time.Hour))
		want := map[store.TriggerLimit]int{store.LimitRate: 2, store.LimitDaily: 1}
		if err != nil || len(ds) != 1 || !reflect.DeepEqual(ds[0].Counts, want) {
			t.Errorf("digests = %+v, %v; want one window counting %v", ds, err, want)
		}
	})

	su.run(t, "a digest is of one trigger", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		for trigger := range 2 {
			for _, ts := range []string{"a", "b"} {
				r := runOf(fmt.Sprint(trigger, ts), t0)
				r.Trigger = trigger
				admit(t, s, r, lim)
			}
		}
		ds, err := s.ListTriggerDigests(t.Context(), t0.Add(2*time.Hour))
		if err != nil || len(ds) != 2 || ds[0].Trigger == ds[1].Trigger || ds[0].Counts[store.LimitRate] != 1 || ds[1].Counts[store.LimitRate] != 1 {
			t.Errorf("digests = %+v, %v; want one window for each of the two triggers", ds, err)
		}
	})

	su.run(t, "the purge of runs deletes what is older than the cutoff", func(t *testing.T, s store.Store) {
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
		admit(t, s, runOf("1.1", t0), lim)
		admit(t, s, runOf("1.2", t0.Add(time.Minute)), lim) // stopped
		admit(t, s, runOf("1.3", t0.Add(25*time.Hour)), lim)
		n, err := s.PurgeTriggerRuns(t.Context(), t0.Add(2*time.Minute))
		if err != nil || n != 2 {
			t.Errorf("PurgeTriggerRuns = %d, %v; want the two older rows", n, err)
		}
		if ds, err := s.ListTriggerDigests(t.Context(), t0.Add(48*time.Hour)); err != nil || len(ds) != 0 {
			t.Errorf("digests after the purge = %+v, %v; want none", ds, err)
		}
	})

	su.run(t, "runs, digests and purges refuse what is invalid", func(t *testing.T, s store.Store) {
		ok := runOf("1.1", t0)
		for name, mutate := range map[string]func(*store.TriggerRun){
			"no Binding":   func(r *store.TriggerRun) { r.Binding = "" },
			"no channel":   func(r *store.TriggerRun) { r.Key.Channel = "" },
			"no thread":    func(r *store.TriggerRun) { r.Key.Thread = "" },
			"no time":      func(r *store.TriggerRun) { r.At = time.Time{} },
			"before 1970":  func(r *store.TriggerRun) { r.At = time.Unix(0, 0) },
			"a trigger -1": func(r *store.TriggerRun) { r.Trigger = -1 },
		} {
			r := ok
			mutate(&r)
			if _, err := s.AdmitTriggerRun(t.Context(), r, runLimits); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("AdmitTriggerRun with %s = %v; want ErrInvalid", name, err)
			}
		}
		if _, err := s.AdmitTriggerRun(t.Context(), ok, store.TriggerRunLimits{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("AdmitTriggerRun with no digest window = %v; want ErrInvalid", err)
		}
		if err := s.MarkTriggerDigested(t.Context(), "", 1, t0); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("MarkTriggerDigested with no Binding = %v; want ErrInvalid", err)
		}
		if _, err := s.ListTriggerDigests(t.Context(), time.Time{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("ListTriggerDigests with no time = %v; want ErrInvalid", err)
		}
		if _, err := s.PurgeTriggerRuns(t.Context(), time.Time{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("PurgeTriggerRuns with no time = %v; want ErrInvalid", err)
		}
	})

}
