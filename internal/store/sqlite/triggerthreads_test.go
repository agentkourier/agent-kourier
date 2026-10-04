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

// opening is the thread a fire at at, of the message ts, would reserve.
func opening(ts string, at time.Time) store.TriggerThread {
	return store.TriggerThread{Binding: tBinding, KeyHash: tKey, Thread: ts, Opened: at}
}

// reserve is OpenTriggerThread with no stale takeover: a reservation whose turn never started stays.
func reserve(t *testing.T, s *Store, th store.TriggerThread) (store.TriggerThread, bool, error) {
	t.Helper()
	return s.OpenTriggerThread(t.Context(), th, time.Time{})
}

// startAt reserves the key's thread for ts at at and marks its turn started, with the cooldown of tCool: what the
// intake does for a fire that starts a turn.
func startAt(t *testing.T, s *Store, ts string, at time.Time) {
	t.Helper()
	if _, opened, err := reserve(t, s, opening(ts, at)); err != nil || !opened {
		t.Fatalf("OpenTriggerThread(%s) = %v, %v; want it reserved", ts, opened, err)
	}
	if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, ts, at.Add(tCool)); err != nil {
		t.Fatal(err)
	}
}

func closeAt(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	if ok, err := s.CloseTriggerThread(t.Context(), tBinding, tKey, at); err != nil || !ok {
		t.Fatalf("CloseTriggerThread = %v, %v; want it closed", ok, err)
	}
}

// A key with no row reserves a thread, which comes back open, last seen when it opened, with no cooldown: its
// turn has not started.
func TestOpenTriggerThreadReservesThreadForNewKey(t *testing.T) {
	s := openTemp(t)
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
}

// What the caller puts in the fields the row owns is not the row's: a thread starts open, last seen as it opened,
// and with no cooldown until it is marked.
func TestOpenTriggerThreadIgnoresTheFieldsTheRowOwns(t *testing.T) {
	s := openTemp(t)
	in := opening("100.1", t0)
	in.LastSeen, in.ClosedAt, in.CooldownUntil = t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(tCool)
	got, _, err := reserve(t, s, in)
	if err != nil || !got.LastSeen.Equal(t0) || !got.ClosedAt.IsZero() || got.Started() {
		t.Errorf("OpenTriggerThread = %+v, %v; want last seen %v, not closed, not started", got, err, t0)
	}
}

// Marking starts the cooldown, once, for the thread that was reserved.
func TestMarkTriggerStartedStartsTheCooldown(t *testing.T) {
	s := openTemp(t)
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
}

func TestMarkTriggerStartedRefusesWhatIsNotTheKeysThread(t *testing.T) {
	s := openTemp(t)
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
}

// A key with a started thread, open or closed inside its cooldown, reserves none: it gets the row that is there.
func TestOpenTriggerThreadReturnsTheExistingThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closed bool
		at     time.Time
	}{
		{"open, inside the cooldown", false, t0.Add(time.Hour)},
		{"open, past the cooldown", false, t0.Add(tCool + time.Hour)},
		{"closed, inside the cooldown", true, t0.Add(time.Hour)},
		{"closed, one tick before the cooldown ends", true, t0.Add(tCool - time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
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
}

// A closed thread whose cooldown has ended is replaced by a new one, which is open, with no cooldown until its
// own turn starts.
func TestOpenTriggerThreadReplacesAClosedThreadPastItsCooldown(t *testing.T) {
	for name, at := range map[string]time.Time{"at the end of the cooldown": t0.Add(tCool), "after it": t0.Add(tCool + time.Hour)} {
		t.Run(name, func(t *testing.T) {
			s := openTemp(t)
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
}

// A turn that never started holds nothing once its reservation is stale: a later message of the key takes it over,
// and the first one's turn, if it comes late, is refused its mark. One that is not stale yet may be a turn that
// is starting, and stays; so does a started one, however old. The same message finds its own reservation.
func TestOpenTriggerThreadTakesOverAReservationThatNeverStarted(t *testing.T) {
	const grace = time.Minute
	s := openTemp(t)
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
		"another message, a tick before the grace ends": {"200.2", t0.Add(grace - time.Nanosecond)},
	} {
		got, opened, err := s.OpenTriggerThread(t.Context(), opening(tc.ts, tc.at), tc.at.Add(-grace))
		if err != nil || opened || got.Thread != "100.1" || !got.Opened.Equal(t0) {
			t.Errorf("%s: %+v, opened %v, %v; want the first reservation back", name, got, opened, err)
		}
	}
	at := t0.Add(grace + time.Nanosecond)
	got, opened, err := s.OpenTriggerThread(t.Context(), opening("200.2", at), at.Add(-grace))
	if err != nil || !opened || got.Thread != "200.2" || !got.Opened.Equal(at) {
		t.Fatalf("another message, past the grace: %+v, opened %v, %v; want the reservation taken over", got, opened, err)
	}
	if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "100.1", t0.Add(tCool)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("the late mark of the first message: %v; want ErrConflict", err)
	}
	if err := s.MarkTriggerStarted(t.Context(), tBinding, tKey, "200.2", at.Add(tCool)); err != nil {
		t.Fatal(err)
	}
	if _, opened, err := s.OpenTriggerThread(t.Context(), opening("300.3", at.Add(24*time.Hour)), at.Add(23*time.Hour)); err != nil || opened {
		t.Errorf("a started thread, a day on, an open one: opened %v, %v; want it kept", opened, err)
	}
}

// The second delivery of the message that reserved a thread finds its own row, which is how the caller knows
// it for a redelivery.
func TestOpenTriggerThreadOfTheSameMessageFindsItsOwnRow(t *testing.T) {
	s := openTemp(t)
	for i := range 2 {
		got, opened, err := reserve(t, s, opening("100.1", t0))
		if err != nil || opened != (i == 0) || got.Thread != "100.1" {
			t.Fatalf("delivery %d: %+v, opened %v, %v", i, got, opened, err)
		}
	}
}

// Two starts of one key at once: exactly one wins, and the rest see its row, which, reserved a moment ago and not
// yet started, is not stale.
func TestOpenTriggerThreadHasOneWinnerUnderConcurrency(t *testing.T) {
	s := openTemp(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts := "100." + string(rune('a'+i))
			if _, opened, err := s.OpenTriggerThread(t.Context(), opening(ts, t0), t0.Add(-time.Minute)); err != nil {
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
}

// A thread is closed once, at its first close time, and the key's row stays for the cooldown.
func TestCloseTriggerThreadClosesOnceAndKeepsTheFirstTime(t *testing.T) {
	s := openTemp(t)
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
}

// A fire again moves last seen forward and reopens the thread, so the next resolve is a close; an older time
// moves nothing back, and the cooldown is the start's.
func TestReopenTriggerThreadReopensAndOnlyMovesLastSeenForward(t *testing.T) {
	s := openTemp(t)
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
}

// The mapping is the row, so a restart, a reopened database, loses nothing: the thread, the close and the cooldown.
func TestTriggerThreadsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	startAt(t, s, "100.1", t0)
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
	if _, opened, err := reserve(t, s, opening("200.2", t0.Add(time.Hour))); err != nil || opened {
		t.Errorf("a start inside the cooldown after the restart: opened %v, %v; want the cooldown kept", opened, err)
	}
}
