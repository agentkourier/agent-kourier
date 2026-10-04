package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	rConn    = "courier/slack-main"
	rChannel = "C0ALERTS"
)

// runLimits is the preset's limits with a 10 minute window: five starts in it, two at a time, 40 a day.
var runLimits = store.TriggerRunLimits{MaxRuns: 5, Per: 10 * time.Minute, MaxConcurrent: 2, MaxPerDay: 40, DigestWindow: 10 * time.Minute}

// runOf is the start of the alert message ts by trigger 1 of the Binding, decided at at.
func runOf(ts string, at time.Time) store.TriggerRun {
	return store.TriggerRun{Binding: tBinding, Trigger: 1, Key: store.ConversationKey{Connection: rConn, Channel: rChannel, Thread: ts}, At: at}
}

// admit asks for the start and requires the store to answer.
func admit(t *testing.T, s *Store, run store.TriggerRun, lim store.TriggerRunLimits) store.TriggerAdmission {
	t.Helper()
	got, err := s.AdmitTriggerRun(t.Context(), run, lim)
	if err != nil {
		t.Fatalf("AdmitTriggerRun(%s) = %v", run.Key.Thread, err)
	}
	return got
}

// running makes the alert message's session have an active task, as the manager does once the agent has taken the turn.
func running(t *testing.T, s *Store, ts string, at time.Time) {
	t.Helper()
	key := runOf(ts, at).Key
	if err := s.UpsertSession(t.Context(), store.Session{Key: key, Binding: tBinding, TaskID: "task-" + ts, LastActivity: at}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitTriggerRunAdmitsUnderTheLimits(t *testing.T) {
	s := openTemp(t)
	if got := admit(t, s, runOf("100.1", t0), runLimits); !got.Admitted || got.Limit != "" {
		t.Errorf("the first start = %+v; want admitted", got)
	}
}

func TestAdmitTriggerRunLimitsTheRateInASlidingWindow(t *testing.T) {
	s := openTemp(t)
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
		{"just before the first leaves the window", 10*time.Minute - time.Nanosecond, store.TriggerAdmission{Limit: store.LimitRate}},
		{"when the first is exactly a window old it no longer counts", 10 * time.Minute, store.TriggerAdmission{Admitted: true}},
	} {
		if got := admit(t, s, runOf("2."+tt.name, t0.Add(tt.at)), lim); got != tt.want {
			t.Errorf("%s: %+v; want %+v", tt.name, got, tt.want)
		}
	}
}

func TestAdmitTriggerRunLimitsTheDay(t *testing.T) {
	s := openTemp(t)
	lim := store.TriggerRunLimits{MaxPerDay: 2, DigestWindow: time.Hour}
	admit(t, s, runOf("1.1", t0), lim)
	admit(t, s, runOf("1.2", t0.Add(12*time.Hour)), lim)
	if got := admit(t, s, runOf("1.3", t0.Add(23*time.Hour)), lim); got.Admitted || got.Limit != store.LimitDaily {
		t.Errorf("the third in a day = %+v; want daily", got)
	}
	if got := admit(t, s, runOf("1.4", t0.Add(24*time.Hour)), lim); !got.Admitted {
		t.Errorf("a start when the first is a day old = %+v; want admitted", got)
	}
}

func TestAdmitTriggerRunLimitsConcurrentInvestigations(t *testing.T) {
	s := openTemp(t)
	lim := store.TriggerRunLimits{MaxConcurrent: 2, DigestWindow: time.Hour}
	admit(t, s, runOf("1.1", t0), lim)
	admit(t, s, runOf("1.2", t0.Add(time.Second)), lim)
	running(t, s, "1.1", t0)
	running(t, s, "1.2", t0)

	if got := admit(t, s, runOf("1.3", t0.Add(2*time.Second)), lim); got.Admitted || got.Limit != store.LimitConcurrent {
		t.Fatalf("a third with two running = %+v; want concurrent", got)
	}
	// One ends: the session's task is cleared, as the manager does when the task ends.
	if err := s.SetSessionTask(t.Context(), runOf("1.1", t0).Key, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := admit(t, s, runOf("1.3", t0.Add(3*time.Second)), lim); !got.Admitted {
		t.Errorf("a start when one of the two has ended = %+v; want admitted", got)
	}
}

// A start that was queued and has not begun is running as far as the limit goes; otherwise a burst that arrives
// faster than the agent takes the first turn would all pass.
func TestAdmitTriggerRunCountsAQueuedTurnAsRunning(t *testing.T) {
	s := openTemp(t)
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
}

// A person's reply in the thread of a running investigation is no second investigation, and one session counts once.
func TestAdmitTriggerRunCountsOneSessionOnce(t *testing.T) {
	s := openTemp(t)
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
}

func TestAdmitTriggerRunReportsTheLongestLastingLimitFirst(t *testing.T) {
	s := openTemp(t)
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
}

func TestAdmitTriggerRunCountsEachBindingAndTriggerOnItsOwn(t *testing.T) {
	s := openTemp(t)
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
}

// Asking for a message again, as a redelivered event does, is the same start: it is admitted and counted once, even
// when the limit has been reached by then.
func TestAdmitTriggerRunIsIdempotentByMessage(t *testing.T) {
	s := openTemp(t)
	lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: time.Hour}
	for range 3 {
		if got := admit(t, s, runOf("1.1", t0), lim); !got.Admitted {
			t.Fatalf("the same message again = %+v; want admitted", got)
		}
	}
	if got := admit(t, s, runOf("1.2", t0.Add(time.Second)), lim); got.Admitted {
		t.Errorf("another message after one was asked three times = %+v; want rate", got)
	}
}

// A message that was stopped is checked afresh when it is asked for again, so a redelivery after capacity came back
// starts, and its row stops being a stopped one: the digest does not count a start.
func TestAdmitTriggerRunChecksAStoppedMessageAgain(t *testing.T) {
	s := openTemp(t)
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
}

// The counts are the store's, not the process's: they hold after a restart.
func TestTriggerRunLimitsSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	lim := store.TriggerRunLimits{MaxRuns: 2, Per: time.Hour, DigestWindow: time.Hour}
	s := openAt(t, path)
	admit(t, s, runOf("1.1", t0), lim)
	admit(t, s, runOf("1.2", t0), lim)
	if got := admit(t, s, runOf("1.3", t0), lim); got.Admitted {
		t.Fatal("the third was admitted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path)
	if got := admit(t, s, runOf("1.4", t0.Add(time.Minute)), lim); got.Admitted || got.Limit != store.LimitRate {
		t.Errorf("a start after a reopen = %+v; want rate", got)
	}
	if ds, err := s.ListTriggerDigests(t.Context(), t0.Add(2*time.Hour)); err != nil || len(ds) != 1 || ds[0].Counts[store.LimitRate] != 2 {
		t.Errorf("digests after a reopen = %+v, %v; want the two stopped starts", ds, err)
	}
}

func TestTriggerDigestWindowsOpenAtTheFirstStoppedStart(t *testing.T) {
	s := openTemp(t)
	lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: 10 * time.Minute}
	admit(t, s, runOf("1.0", t0), lim)
	// A burst that begins a while after the start: its window opens with its first stopped start, on no clock's boundary.
	first := t0.Add(7 * time.Minute)
	for i, d := range []time.Duration{0, time.Second, 9 * time.Minute} {
		admit(t, s, runOf(fmt.Sprint("2.", i), first.Add(d)), lim)
	}
	later := first.Add(10*time.Minute + time.Second) // after the window that opened at first
	admit(t, s, runOf("3.0", later), lim)

	if ds, err := s.ListTriggerDigests(t.Context(), first.Add(10*time.Minute-time.Nanosecond)); err != nil || len(ds) != 0 {
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
}

func TestTriggerDigestCountsByLimit(t *testing.T) {
	s := openTemp(t)
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
}

func TestTriggerDigestsAreOfOneTrigger(t *testing.T) {
	s := openTemp(t)
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
}

func TestPurgeTriggerRunsDeletesWhatIsOlder(t *testing.T) {
	s := openTemp(t)
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
}

func TestTriggerRunsRefuseWhatIsInvalid(t *testing.T) {
	s := openTemp(t)
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
}
