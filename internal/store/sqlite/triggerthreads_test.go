package sqlite

import (
	"errors"
	"path/filepath"
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
)

// opening is the thread a fire at at, of the message ts, would start.
func opening(ts string, at time.Time) store.TriggerThread {
	return store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: ts, Opened: at, CooldownUntil: at.Add(tCool)}
}

// A key with no row starts a thread, which comes back open, last seen when it opened.
func TestOpenTriggerThreadStartsThreadForNewKey(t *testing.T) {
	s := openTemp(t)
	got, opened, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0))
	if err != nil || !opened {
		t.Fatalf("OpenTriggerThread = %+v, %v, %v; want it opened", got, opened, err)
	}
	want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "100.1", Opened: t0, LastSeen: t0, CooldownUntil: t0.Add(tCool)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("thread = %+v; want %+v", got, want)
	}
	if again, err := s.GetTriggerThread(t.Context(), tBinding, tKey); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("GetTriggerThread = %+v, %v; want %+v", again, err, want)
	}
}

// What the caller puts in LastSeen and ClosedAt is not the row's: a thread starts open, last seen as it opened.
func TestOpenTriggerThreadIgnoresLastSeenAndClosedAt(t *testing.T) {
	s := openTemp(t)
	in := opening("100.1", t0)
	in.LastSeen, in.ClosedAt = t0.Add(time.Hour), t0.Add(2*time.Hour)
	got, _, err := s.OpenTriggerThread(t.Context(), in)
	if err != nil || !got.LastSeen.Equal(t0) || !got.ClosedAt.IsZero() {
		t.Errorf("OpenTriggerThread = %+v, %v; want last seen %v and not closed", got, err, t0)
	}
}

// A key with a thread, open or closed inside its cooldown, starts none: it gets the row that is there.
func TestOpenTriggerThreadReturnsTheExistingThread(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prep  func(*Store)
		at    time.Time
		wantC bool // the row is closed
	}{
		{"open, inside the cooldown", func(*Store) {}, t0.Add(time.Hour), false},
		{"open, past the cooldown", func(*Store) {}, t0.Add(tCool + time.Hour), false},
		{"closed, inside the cooldown", func(s *Store) { closeAt(t, s, t0.Add(time.Minute)) }, t0.Add(time.Hour), true},
		{"closed, one tick before the cooldown ends", func(s *Store) { closeAt(t, s, t0.Add(time.Minute)) }, t0.Add(tCool - time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			if _, _, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0)); err != nil {
				t.Fatal(err)
			}
			tc.prep(s)
			got, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", tc.at))
			if err != nil || opened {
				t.Fatalf("OpenTriggerThread = %+v, %v, %v; want the existing thread and no start", got, opened, err)
			}
			if got.Thread != "100.1" || !got.Opened.Equal(t0) || got.ClosedAt.IsZero() == tc.wantC {
				t.Errorf("row = %+v; want the first thread, untouched (closed: %v)", got, tc.wantC)
			}
		})
	}
}

func closeAt(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, at); err != nil || !ok {
		t.Fatalf("CloseTriggerThread = %v, %v; want it closed", ok, err)
	}
}

// A closed thread whose cooldown has ended is replaced by a new one, which is open with a cooldown of its own.
func TestOpenTriggerThreadReplacesAClosedThreadPastItsCooldown(t *testing.T) {
	for name, at := range map[string]time.Time{"at the end of the cooldown": t0.Add(tCool), "after it": t0.Add(tCool + time.Hour)} {
		t.Run(name, func(t *testing.T) {
			s := openTemp(t)
			if _, _, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0)); err != nil {
				t.Fatal(err)
			}
			closeAt(t, s, t0.Add(time.Minute))
			got, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", at))
			if err != nil || !opened {
				t.Fatalf("OpenTriggerThread = %+v, %v, %v; want a new thread", got, opened, err)
			}
			want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "200.2", Opened: at, LastSeen: at, CooldownUntil: at.Add(tCool)}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("thread = %+v; want %+v", got, want)
			}
		})
	}
}

// The second delivery of the message that started a thread finds its own row, which is how the caller knows
// it for a redelivery.
func TestOpenTriggerThreadOfTheSameMessageFindsItsOwnRow(t *testing.T) {
	s := openTemp(t)
	for i := range 2 {
		got, opened, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0))
		if err != nil || opened != (i == 0) || got.Thread != "100.1" {
			t.Fatalf("delivery %d: %+v, opened %v, %v", i, got, opened, err)
		}
	}
}

// Two starts of one key at once: exactly one wins, and the rest see its row.
func TestOpenTriggerThreadHasOneWinnerUnderConcurrency(t *testing.T) {
	s := openTemp(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts := "100." + string(rune('a'+i))
			if _, opened, err := s.OpenTriggerThread(t.Context(), opening(ts, t0)); err != nil {
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
}

// The binding and the key are the row's identity: other keys and other Bindings have threads of their own.
func TestTriggerThreadsAreKeyedByBindingAndKey(t *testing.T) {
	s := openTemp(t)
	for i, k := range []struct{ binding, key string }{{tBinding, "k1"}, {tBinding, "k2"}, {"payments/pay", "k1"}} {
		in := opening("100."+string(rune('1'+i)), t0)
		in.Binding, in.KeyHash = k.binding, k.key
		if _, opened, err := s.OpenTriggerThread(t.Context(), in); err != nil || !opened {
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
}

// A thread is closed once, at its first close time, and the key's row stays for the cooldown.
func TestCloseTriggerThreadClosesOnceAndKeepsTheFirstTime(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0)); err != nil {
		t.Fatal(err)
	}
	first := t0.Add(10 * time.Minute)
	closeAt(t, s, first)
	if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, first.Add(time.Hour)); err != nil || ok {
		t.Errorf("second close = %v, %v; want false, nil", ok, err)
	}
	got, err := s.GetTriggerThread(t.Context(), tBinding, tKey)
	if err != nil || !got.ClosedAt.Equal(first) || !got.LastSeen.Equal(first) || got.Thread != "100.1" {
		t.Errorf("row = %+v, %v; want closed at %v, last seen then, same thread", got, err, first)
	}
}

// A fire again moves last seen forward and reopens the thread, so the next resolve is a close; an older time
// moves nothing back.
func TestReopenTriggerThreadReopensAndOnlyMovesLastSeenForward(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0)); err != nil {
		t.Fatal(err)
	}
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
}

func TestTriggerThreadOperationsOnAKeyWithNoRowAreNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetTriggerThread(t.Context(), tBinding, tKey); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTriggerThread: %v; want ErrNotFound", err)
	}
	if err := s.ReopenTriggerThread(t.Context(), tBinding, tKey, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ReopenTriggerThread: %v; want ErrNotFound", err)
	}
	if _, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CloseTriggerThread: %v; want ErrNotFound", err)
	}
}

func TestTriggerThreadOperationsRefuseIncompleteArguments(t *testing.T) {
	s := openTemp(t)
	for name, mutate := range map[string]func(*store.TriggerThread){
		"no binding": func(t *store.TriggerThread) { t.Binding = "" }, "no key hash": func(t *store.TriggerThread) { t.KeyHash = "" },
		"no thread": func(t *store.TriggerThread) { t.Thread = "" }, "no time": func(t *store.TriggerThread) { t.Opened = time.Time{} },
		"a cooldown that ends before the start": func(t *store.TriggerThread) { t.CooldownUntil = t.Opened.Add(-time.Second) },
	} {
		in := opening("100.1", t0)
		mutate(&in)
		if _, _, err := s.OpenTriggerThread(t.Context(), in); !errors.Is(err, store.ErrInvalid) {
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
}

// The mapping is the row, so a restart, a reopened database, loses nothing: the thread, the close and the cooldown.
func TestTriggerThreadsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	if _, _, err := s.OpenTriggerThread(t.Context(), opening("100.1", t0)); err != nil {
		t.Fatal(err)
	}
	closeAt(t, s, t0.Add(10*time.Minute))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	got, err := s.GetTriggerThread(t.Context(), tBinding, tKey)
	want := store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: "100.1", Opened: t0, LastSeen: t0.Add(10 * time.Minute), ClosedAt: t0.Add(10 * time.Minute), CooldownUntil: t0.Add(tCool)}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after reopening: %+v, %v; want %+v", got, err, want)
	}
	if _, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", t0.Add(time.Hour))); err != nil || opened {
		t.Errorf("a start inside the cooldown after the restart: opened %v, %v; want the cooldown kept", opened, err)
	}
}
