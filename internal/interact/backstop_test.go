package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// flakySessions fails the next reads of a session.
type flakySessions struct {
	interact.SessionReader
	fail int
}

func (f *flakySessions) GetSession(ctx context.Context, k store.ConversationKey, ttl time.Duration, now time.Time) (store.Session, error) {
	if f.fail > 0 {
		f.fail--
		return store.Session{}, errBoom
	}
	return f.SessionReader.GetSession(ctx, k, ttl, now)
}

// countedOutbox counts the notices the backstop asks for.
type countedOutbox struct {
	*outbox.Outbox
	calls int
}

func (c *countedOutbox) Enqueue(ctx context.Context, p outbox.Post) (string, error) {
	c.calls++
	return c.Outbox.Enqueue(ctx, p)
}

// backstopRig is a store with interactions in it, the real outbox that takes the notices (its Slack is
// never called: a notice is only enqueued here), and a clock.
type backstopRig struct {
	st   *sqlite.Store
	ob   *countedOutbox
	sess *flakySessions
	now  time.Time
}

func newBackstopRig(t *testing.T) *backstopRig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &backstopRig{st: st, sess: &flakySessions{SessionReader: st}, now: d0.Add(time.Hour)}
	r.ob = &countedOutbox{Outbox: outbox.New(outbox.Config{Store: st, Platform: &poster{posted: map[string]string{}}, Now: func() time.Time { return r.now }, CallTimeout: time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})}
	return r
}

func (r *backstopRig) backstop(t *testing.T) *interact.Backstop {
	t.Helper()
	b, err := interact.NewBackstop(interact.BackstopConfig{Store: r.st, Sessions: r.sess, Outbox: r.ob, Now: func() time.Time { return r.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Grace: time.Minute, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// answered makes an interaction answered at d0+1m, for task, in the thread of key, undelivered.
func (r *backstopRig) answered(t *testing.T, id, task string) {
	t.Helper()
	req, _ := json.Marshal(question)
	k := key
	k.Thread = id // a thread of its own, so each has its own session
	row := store.Interaction{ID: id, Key: k, TaskID: task, Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SubmitInteraction(t.Context(), id, []byte(`{}`), "UALICE1", d0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// session records the task the thread of id is on, "" once it has ended.
func (r *backstopRig) session(t *testing.T, id, task string) {
	t.Helper()
	k := key
	k.Thread = id
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: k, ContextID: "c1", TaskID: task, Binding: "ns/b", LastActivity: r.now}); err != nil {
		t.Fatal(err)
	}
}

// notice is the notice of id, if one was enqueued.
func (r *backstopRig) notice(t *testing.T, id string) (store.OutboxEntry, bool) {
	t.Helper()
	row, err := r.st.GetOutbox(t.Context(), outbox.ID(id+"/answer", store.OutboxNotice))
	if errors.Is(err, store.ErrNotFound) {
		return store.OutboxEntry{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

// An answer that never reached the agent, for a task that has since ended, is told to the thread: once,
// with an ID from the interaction, however often the backstop looks, and however many processes have
// looked, so that the notice the decider posts for the same answer is the same row.
func TestAnEndedTaskWithAnUndeliveredAnswerGetsOneNotice(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "") // the task ended
	b := r.backstop(t)
	if err := b.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, ok := r.notice(t, "ix-1")
	if !ok || row.Role != store.OutboxNotice || row.Channel != key.Channel || row.Thread != "ix-1" {
		t.Fatalf("notice = %+v, %v; want one in the thread of the interaction", row, ok)
	}
	if want, err := json.Marshal(chat.Say(chat.Message{Fallback: closedBeforeText})); err != nil || row.Payload != string(want) {
		t.Fatalf("notice payload = %s; want the same neutral wording as the decider's", row.Payload)
	}
	// Looked at again, the process asks for it no more; and by a process that has just started, it asks
	// again and the outbox makes it the one row.
	_ = b.Check(t.Context())
	if r.ob.calls != 1 {
		t.Errorf("%d notices asked for by one backstop; want one", r.ob.calls)
	}
	_ = r.backstop(t).Check(t.Context())
	if unposted, _ := r.st.ListUnposted(t.Context()); len(unposted) != 1 || r.ob.calls != 2 {
		t.Errorf("%d notices waiting after %d requests; want one row", len(unposted), r.ob.calls)
	}
}

func TestADeliveredAnswerGetsNoNotice(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "")
	if err := r.st.MarkInteractionDelivered(t.Context(), "ix-1", d0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = r.backstop(t).Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); ok {
		t.Error("a notice for an answer that was delivered")
	}
}

// A task the session still records is running, or waiting to be re-parked on, and may yet take the
// answer: it is told nothing until it has ended.
func TestATaskStillRunningGetsNoNotice(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "task-1")
	b := r.backstop(t)
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); ok {
		t.Fatal("a notice while the task is still the session's")
	}
	r.session(t, "ix-1", "") // now it ended
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); !ok {
		t.Error("no notice once the task ended")
	}
	// Another task taking its place is the same: the one the answer was for is over.
	r.answered(t, "ix-2", "task-1")
	r.session(t, "ix-2", "task-9")
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-2"); !ok {
		t.Error("no notice when another task has taken the task's place")
	}
}

// A thread whose session is gone has no task that could take the answer.
func TestAThreadWithNoSessionGetsTheNotice(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	_ = r.backstop(t).Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); !ok {
		t.Error("no notice for a thread whose session is gone")
	}
}

// An answer decided a moment ago may be on its way; the backstop waits out its grace, and does not look
// past the horizon.
func TestTheBackstopWaitsOutTheGraceAndStopsAtTheHorizon(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "")
	b := r.backstop(t)
	r.now = d0.Add(time.Minute + 59*time.Second) // decided 59 s ago
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); ok {
		t.Fatal("a notice inside the grace")
	}
	r.now = d0.Add(2 * time.Minute) // a minute after
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-1"); !ok {
		t.Fatal("no notice once the grace had passed")
	}
	r.answered(t, "ix-old", "task-1")
	r.session(t, "ix-old", "")
	r.now = d0.Add(26 * time.Hour) // past the 24 hour horizon
	_ = b.Check(t.Context())
	if _, ok := r.notice(t, "ix-old"); ok {
		t.Error("a notice for an answer decided more than a horizon ago")
	}
}

// Only an answered interaction has an answer: pending, timed-out and cancelled ones have none to lose.
func TestOnlyAnAnsweredInteractionIsLookedAt(t *testing.T) {
	r := newBackstopRig(t)
	req, _ := json.Marshal(question)
	for id, state := range map[string]store.InteractionState{"ix-pending": store.InteractionPending, "ix-timed": store.InteractionTimedOut, "ix-cancelled": store.InteractionCancelled} {
		k := key
		k.Thread = id
		if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: id, Key: k, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}); err != nil {
			t.Fatal(err)
		}
		if state != store.InteractionPending {
			if err := r.st.ResolveInteraction(t.Context(), id, state, deadline); err != nil {
				t.Fatal(err)
			}
		}
		r.session(t, id, "")
	}
	_ = r.backstop(t).Check(t.Context())
	if unposted, _ := r.st.ListUnposted(t.Context()); len(unposted) != 0 {
		t.Errorf("%d notices for interactions that were not answered", len(unposted))
	}
}

// A session that cannot be read says nothing, and is looked at again: the error is returned, and the
// others are not held up.
func TestASessionThatCannotBeReadIsTriedAgain(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "")
	r.sess.fail = 1
	b := r.backstop(t)
	if err := b.Check(t.Context()); !errors.Is(err, errBoom) {
		t.Fatalf("Check = %v; want the read error", err)
	}
	if _, ok := r.notice(t, "ix-1"); ok {
		t.Fatal("a notice when the session could not be read")
	}
	if err := b.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.notice(t, "ix-1"); !ok {
		t.Error("no notice once the session could be read")
	}
}

func TestRunChecksAtOnceAndEndsWithItsContext(t *testing.T) {
	r := newBackstopRig(t)
	r.answered(t, "ix-1", "task-1")
	r.session(t, "ix-1", "")
	before := goleak.IgnoreCurrent()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.backstop(t).Run(ctx) }()
	waitFor(t, "the notice", func() bool { _, ok := r.notice(t, "ix-1"); return ok })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v; want the context's error", err)
	}
	goleak.VerifyNone(t, before)
}

func TestNewBackstopNeedsItsParts(t *testing.T) {
	if _, err := interact.NewBackstop(interact.BackstopConfig{}); err == nil {
		t.Error("NewBackstop accepted an empty config")
	}
}
