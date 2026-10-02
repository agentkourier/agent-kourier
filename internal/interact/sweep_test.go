package interact_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// callLog records the order of the sweep's calls on its fakes. A nil log records nothing.
type callLog struct {
	mu  sync.Mutex
	got []string
}

func (l *callLog) add(call string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, call)
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

// logBuf is a log destination that a test may read while the sweeper writes.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// sweepSess is a session manager whose conversation is paused on one task: a cancel of any other
// is ErrNotPaused, as the real one answers. errs are returned by the next Cancels, one each.
type sweepSess struct {
	mu      sync.Mutex
	current a2a.TaskID
	cancels []a2a.TaskID
	errs    []error
	always  error
	log     *callLog
}

func (s *sweepSess) Answer(context.Context, store.ConversationKey, *a2a.Message) error { return nil }

func (s *sweepSess) Cancel(_ context.Context, _ store.ConversationKey, id a2a.TaskID, _ string) error {
	s.log.add("cancel")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels = append(s.cancels, id)
	switch {
	case s.always != nil:
		return s.always
	case len(s.errs) > 0:
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	case id != s.current:
		return session.ErrNotPaused
	}
	s.current = ""
	return nil
}

func (s *sweepSess) cancelled() []a2a.TaskID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]a2a.TaskID(nil), s.cancels...)
}

// updates records the prompt replacements; err fails the next ones.
type updates struct {
	mu   sync.Mutex
	got  []string // "channel ts outcome"
	ends []chat.Ending
	err  error
	log  *callLog
}

func (u *updates) Update(_ context.Context, r chat.Ref, o chat.Outgoing) error {
	u.log.add("edit")
	u.mu.Lock()
	defer u.mu.Unlock()
	if o.Ending == nil {
		return fmt.Errorf("the edit is not an ending: %+v", o)
	}
	u.got = append(u.got, r.Channel+" "+r.ID+" "+string(o.Ending.Outcome))
	u.ends = append(u.ends, *o.Ending)
	return u.err
}

func (*updates) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: "fake", Channel: channel, Thread: thread, ID: id}
}

func (u *updates) list() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.got...)
}

// audits records the decisions written.
type audits struct {
	mu  sync.Mutex
	got []audit.Decision
	act []audit.Actor
	err error
}

func (a *audits) UnusablePause(context.Context, audit.Actor, a2a.TaskID, string, error) error {
	return nil
}

func (a *audits) UnusablePauseCancel(context.Context, audit.Actor, a2a.TaskID, string, error) error {
	return nil
}

func (a *audits) Decision(_ context.Context, actor audit.Actor, d audit.Decision) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got, a.act = append(a.got, d), append(a.act, actor)
	return a.err
}

// presses answers the sweep's question whether a press is waiting to be decided.
type presses struct {
	mu      sync.Mutex
	waiting map[string]bool
	asked   []string // "id before"
	err     error
}

func (p *presses) PendingPresses(_ context.Context, id string, before time.Time) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, id+" "+before.Format(time.RFC3339))
	return p.waiting[id], p.err
}

// flakyResolve is a store whose next ResolveInteraction fails, as if the process had died before it.
type flakyResolve struct {
	*sqlite.Store
	fail     bool
	conflict bool // the interaction was answered after the sweep listed it
	log      *callLog
}

func (s *flakyResolve) ResolveInteraction(ctx context.Context, id string, state store.InteractionState, now time.Time) error {
	s.log.add("resolve")
	if s.conflict {
		return store.ErrConflict
	}
	if s.fail {
		s.fail = false
		return errBoom
	}
	return s.Store.ResolveInteraction(ctx, id, state, now)
}

// flakyEnqueue fails its first n Enqueues.
type flakyEnqueue struct {
	*outbox.Outbox
	n   int
	log *callLog
}

func (f *flakyEnqueue) Enqueue(ctx context.Context, p outbox.Post) (string, error) {
	f.log.add("notice")
	if f.n > 0 {
		f.n--
		return "", errBoom
	}
	return f.Outbox.Enqueue(ctx, p)
}

type sweepRig struct {
	st    *flakyResolve
	ob    *outbox.Outbox
	sess  *sweepSess
	upd   *updates
	aud   *audits
	clock *time.Time // what the sweeper's clock says; due rows are due by it
	mu    sync.Mutex
	hooks []string
	enq   *flakyEnqueue
	press *presses
	calls *callLog
	logs  *logBuf
}

func (r *sweepRig) sweeper(t *testing.T, mod func(*interact.SweepConfig)) *interact.Sweeper {
	t.Helper()
	cfg := interact.SweepConfig{
		Store: r.st, Sessions: r.st, Session: r.sess, Updater: r.upd, Outbox: r.enq, Auditor: r.aud, Presses: r.press,
		Now:       func() time.Time { return *r.clock },
		Logger:    slog.New(slog.NewTextHandler(r.logs, nil)),
		OnTimeout: func(id string) { r.mu.Lock(); r.hooks = append(r.hooks, id); r.mu.Unlock() },
	}
	if mod != nil {
		mod(&cfg)
	}
	sw, err := interact.NewSweeper(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sw
}

func (r *sweepRig) hooked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hooks...)
}

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := t0.Add(timeout + time.Minute) // a minute past the deadline of a pause made at t0
	r := &sweepRig{st: &flakyResolve{Store: st}, sess: &sweepSess{current: "task-1"}, upd: &updates{}, aud: &audits{}, clock: &now, press: &presses{waiting: map[string]bool{}}}
	r.ob = outbox.New(outbox.Config{Store: st, Platform: &poster{posted: map[string]string{}}, Now: func() time.Time { return t0 }, CallTimeout: 30 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.enq = &flakyEnqueue{Outbox: r.ob}
	r.calls, r.logs = &callLog{}, &logBuf{}
	r.sess.log, r.upd.log, r.enq.log, r.st.log = r.calls, r.calls, r.calls, r.calls
	if err := st.UpsertSession(t.Context(), store.Session{Key: key, ContextID: "ctx-1", TaskID: "task-1", Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	return r
}

// pending adds a pending interaction of a task, due at deadline, with its prompt posted as ref "100.001".
func (r *sweepRig) pending(t *testing.T, id, task string, deadline time.Time) {
	t.Helper()
	req, _ := json.Marshal(dialect.Interaction{Kind: dialect.AskUser, TaskID: a2a.TaskID(task), RequestID: "req-" + id,
		Questions: []dialect.Question{{Text: "Which?", Choices: []string{"a", "b"}}}})
	row := store.Interaction{ID: id, Key: key, TaskID: task, Kind: store.InteractionAskUser, Request: req, CreatedAt: t0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.001"); err != nil {
		t.Fatal(err)
	}
}

func (r *sweepRig) state(t *testing.T, id string) store.InteractionState {
	t.Helper()
	row, err := r.st.GetInteraction(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row.State
}

func (r *sweepRig) notices(t *testing.T) []store.OutboxEntry {
	t.Helper()
	rows, err := r.st.ListUnposted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSweepTimesOutADueInteractionOnce(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.pending(t, "ix-future", "task-2", t0.Add(timeout+time.Hour))
	r.pending(t, "ix-grace", "task-3", r.clock.Add(-time.Second)) // due, but inside the grace for a press that is still being decided
	sw := r.sweeper(t, nil)
	for range 2 { // the second sweep finds nothing to do
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if r.state(t, "ix-1") != store.InteractionTimedOut || r.state(t, "ix-future") != store.InteractionPending || r.state(t, "ix-grace") != store.InteractionPending {
		t.Fatal("only the interaction that is due, past the grace, may time out")
	}
	if got := r.sess.cancelled(); len(got) != 1 || got[0] != "task-1" {
		t.Errorf("cancelled %v; want only task-1, once", got)
	}
	if got := r.upd.list(); len(got) != 1 || got[0] != "C1 100.001 timed_out" {
		t.Errorf("updates %q; want the prompt replaced once", got)
	}
	rows := r.notices(t)
	if len(rows) != 1 || rows[0].ID != outbox.ID("ix-1", store.OutboxNotice) || rows[0].Role != store.OutboxNotice || rows[0].Thread != "1.1" || rows[0].Payload == "" {
		t.Errorf("outbox rows %+v; want one notice in the thread, from the interaction ID", rows)
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result != audit.DecisionTimedOut || r.aud.got[0].InteractionID != "ix-1" || r.aud.got[0].TaskID != "task-1" || r.aud.act[0].Binding != "ns/b" || r.aud.act[0].User != "" {
		t.Errorf("audit %+v by %+v; want one timed_out decision by no person, under the Binding", r.aud.got, r.aud.act)
	}
	if got := r.hooked(); len(got) != 1 || got[0] != "ix-1" {
		t.Errorf("hook calls %q; want one", got)
	}
}

// TestSweepsThatOverlapOrRestartTimeOutOnce: four sweeps at once, and then a process that dies
// before it records the timeout and is replaced by another.
func TestSweepsThatOverlapOrRestartTimeOutOnce(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	sw := r.sweeper(t, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { _ = sw.Sweep(t.Context()) })
	}
	wg.Wait()
	if len(r.aud.got) != 1 || len(r.hooked()) != 1 || len(r.upd.list()) != 1 || len(r.sess.cancelled()) != 1 {
		t.Fatalf("after four overlapping sweeps: %d audits, %d hooks, %d updates, %d cancels; want one each", len(r.aud.got), len(r.hooked()), len(r.upd.list()), len(r.sess.cancelled()))
	}

	r = newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.st.fail = true // the first process cancels, edits and tells the thread, then dies
	if err := r.sweeper(t, nil).Sweep(t.Context()); err == nil {
		t.Fatal("a sweep that could not record the timeout reported success")
	}
	if len(r.aud.got) != 0 || len(r.hooked()) != 0 {
		t.Fatal("a timeout that was not recorded was audited or counted")
	}
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil { // its replacement
		t.Fatal(err)
	}
	if r.state(t, "ix-1") != store.InteractionTimedOut || len(r.aud.got) != 1 || len(r.hooked()) != 1 || len(r.notices(t)) != 1 {
		t.Errorf("after the restart: %d audits, %d hooks, %d notices; want one each, and the interaction timed out", len(r.aud.got), len(r.hooked()), len(r.notices(t)))
	}
}

// TestALateSweepLeavesANewerTaskAlone: the thread has moved on to task-2, which waits on its own
// question that is not yet due. The old question's cancel must name task-1, which is not paused.
func TestALateSweepLeavesANewerTaskAlone(t *testing.T) {
	r := newSweepRig(t)
	r.sess.current = "task-2"
	r.pending(t, "ix-old", "task-1", t0.Add(timeout))
	r.pending(t, "ix-new", "task-2", t0.Add(timeout+time.Hour))
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.sess.cancelled(); len(got) != 1 || got[0] != "task-1" || r.sess.current != "task-2" {
		t.Errorf("cancelled %v, task %q is current; want only task-1 asked for, and task-2 still paused", got, r.sess.current)
	}
	if r.state(t, "ix-old") != store.InteractionTimedOut || r.state(t, "ix-new") != store.InteractionPending || len(r.hooked()) != 1 {
		t.Error("the old question did not time out alone")
	}
}

// TestAStepThatFailsIsRetriedThenGivenUpOn: the edit and the notice leave the interaction pending,
// with nothing audited, until they work; and after five sweeps a step that never will is given up on,
// so one broken prompt does not hold the thread. The cancel is the exception, below.
func TestAStepThatFailsIsRetriedThenGivenUpOn(t *testing.T) {
	for name, tc := range map[string]struct {
		broken func(*sweepRig)
		fixed  func(*sweepRig)
	}{
		"the edit":   {func(r *sweepRig) { r.upd.err = errBoom }, func(r *sweepRig) { r.upd.mu.Lock(); r.upd.err = nil; r.upd.mu.Unlock() }},
		"the notice": {func(r *sweepRig) { r.enq.n = 100 }, func(r *sweepRig) { r.enq.n = 0 }},
	} {
		t.Run(name+" is retried", func(t *testing.T) {
			r := newSweepRig(t)
			r.pending(t, "ix-1", "task-1", t0.Add(timeout))
			tc.broken(r)
			sw := r.sweeper(t, nil)
			if err := sw.Sweep(t.Context()); err == nil {
				t.Fatal("a sweep with a failing step reported success")
			}
			if r.state(t, "ix-1") != store.InteractionPending || len(r.aud.got) != 0 || len(r.hooked()) != 0 {
				t.Fatal("an interaction whose step failed was recorded as timed out")
			}
			tc.fixed(r)
			if err := sw.Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut || len(r.aud.got) != 1 {
				t.Fatalf("the sweep after the fix = %v, state %v, %d audits; want it timed out", err, r.state(t, "ix-1"), len(r.aud.got))
			}
		})
		t.Run(name+" is given up on", func(t *testing.T) {
			r := newSweepRig(t)
			r.pending(t, "ix-1", "task-1", t0.Add(timeout))
			tc.broken(r)
			sw := r.sweeper(t, nil)
			for i := 1; i <= 5; i++ {
				_ = sw.Sweep(t.Context())
				if got, want := r.state(t, "ix-1") == store.InteractionTimedOut, i == 5; got != want {
					t.Fatalf("after sweep %d timed out = %v; want %v", i, got, want)
				}
			}
			if name == "the edit" { // the prompt keeps its controls: findable, for WP-33d3 to repair
				if out := r.logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ix-1") || !strings.Contains(out, "keeps its controls") {
					t.Errorf("log = %q; want a warning naming ix-1 and that its prompt keeps its controls", out)
				}
			}
		})
	}
}

// TestACancelThatNeverWorksIsNeverGivenUpOn: a task that is still paused holds its thread and the
// replies queued behind it, and nothing but this sweep would end it, so a row whose cancel keeps
// failing stays pending however many sweeps fail, with no edit, notice or audit, until it works.
func TestACancelThatNeverWorksIsNeverGivenUpOn(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.sess.always = errBoom
	sw := r.sweeper(t, nil)
	for i := 1; i <= 12; i++ { // far past the five sweeps that give up on the other steps
		if err := sw.Sweep(t.Context()); err == nil {
			t.Fatalf("sweep %d with a failing cancel reported success", i)
		}
		if r.state(t, "ix-1") != store.InteractionPending || len(r.upd.list()) != 0 || len(r.notices(t)) != 0 || len(r.aud.got) != 0 {
			t.Fatalf("after sweep %d the interaction was timed out, edited, noticed or audited with its task still paused", i)
		}
	}
	r.sess.always = nil
	if err := sw.Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut {
		t.Fatalf("the sweep after the fix = %v, state %v; want it timed out", err, r.state(t, "ix-1"))
	}
}

// TestTheStepsRunInOrder: the cancel comes first, so nothing is said or edited for a task that is
// still paused, and the timeout is recorded last, as the commit point that makes the steps before
// it safe to repeat.
func TestTheStepsRunInOrder(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := r.calls.list(), []string{"cancel", "edit", "notice", "resolve"}; !slices.Equal(got, want) {
		t.Errorf("calls %q; want %q", got, want)
	}
}

// TestABacklogIsPacedByTheBatch: after an outage many are due, and a sweep takes only a batch of them,
// so the edits do not go to Slack all at once.
func TestABacklogIsPacedByTheBatch(t *testing.T) {
	r := newSweepRig(t)
	for i := range 25 {
		r.pending(t, fmt.Sprintf("ix-%02d", i), "task-1", t0.Add(timeout+time.Duration(i)*time.Second))
	}
	sw := r.sweeper(t, func(c *interact.SweepConfig) { c.Batch = 10 })
	for _, want := range []int{10, 20, 25} {
		if err := sw.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := len(r.hooked()); got != want {
			t.Fatalf("after a sweep %d timed out; want %d", got, want)
		}
	}
}

// TestAPromptThatWasNeverPostedHasNothingToEdit: no ref, so no edit, and the timeout still counts.
func TestAPromptThatWasNeverPostedHasNothingToEdit(t *testing.T) {
	r := newSweepRig(t)
	req, _ := json.Marshal(dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "r"})
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: "ix-1", Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: t0, Deadline: t0.Add(timeout)}); err != nil {
		t.Fatal(err)
	}
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil || len(r.upd.list()) != 0 || r.state(t, "ix-1") != store.InteractionTimedOut {
		t.Fatalf("Sweep = %v, %d updates, state %v; want no edit and a timeout", err, len(r.upd.list()), r.state(t, "ix-1"))
	}
}

// TestTheTimedOutPromptIsReplacedByAnEndingAtItsDeadline: the edit is the prompt's ending in the neutral
// model, dated at the deadline, with no one named and no answer, and with the question as asked; and a
// request that cannot be read still has one.
func TestTheTimedOutPromptIsReplacedByAnEndingAtItsDeadline(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: "ix-bad", Key: key, TaskID: "task-2", Kind: store.InteractionAskUser, Request: []byte(`not json`), CreatedAt: t0, Deadline: t0.Add(timeout)}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-bad", "100.002"); err != nil {
		t.Fatal(err)
	}
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	ends := r.upd.ends
	if len(ends) != 2 {
		t.Fatalf("%d edits; want both prompts replaced", len(ends))
	}
	for _, e := range ends {
		if e.Outcome != chat.TimedOut || e.By != "" || !e.At.Equal(t0.Add(timeout)) || e.Answers != nil {
			t.Errorf("ending %+v; want timed out at the deadline, with no one named and no answer", e)
		}
	}
	asked := ends[slices.Index(r.upd.got, "C1 100.001 timed_out")] // the order of the two is the store's
	if len(asked.Prompt.Questions) != 1 || asked.Prompt.Questions[0].Text != "Which?" {
		t.Errorf("ending %+v; want the question as asked", asked.Prompt)
	}
}

// TestRunSweepsOnItsInterval ends with its context and leaves no goroutine.
func TestRunSweepsOnItsInterval(t *testing.T) {
	r := newSweepRig(t)
	before := goleak.IgnoreCurrent()
	sw := r.sweeper(t, func(c *interact.SweepConfig) { c.Interval = 5 * time.Millisecond })
	r.pending(t, "ix-1", "task-1", t0.Add(timeout)) // due at the start
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()
	waitFor(t, "the first interaction", func() bool { return len(r.hooked()) == 1 })
	r.pending(t, "ix-2", "task-9", t0.Add(timeout)) // due later, on a tick
	waitFor(t, "the second interaction", func() bool { return len(r.hooked()) == 2 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run returned nil; want the context's error")
	}
	goleak.VerifyNone(t, before)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestAnInteractionAnsweredMeanwhileIsNotReportedAsTimedOut: the answer won the race, so the
// sweep records nothing, audits nothing and counts nothing.
func TestAnInteractionAnsweredMeanwhileIsNotReportedAsTimedOut(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.st.conflict = true
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil || len(r.aud.got) != 0 || len(r.hooked()) != 0 {
		t.Fatalf("Sweep = %v, %d audits, %d hooks; want a clean sweep that reported nothing", err, len(r.aud.got), len(r.hooked()))
	}
}

// TestATimeoutThatCannotBeAuditedStillStands: the audit describes what is done, so its failure is
// logged and never undoes or holds the timeout.
func TestATimeoutThatCannotBeAuditedStillStands(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.aud.err = errBoom
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut || len(r.hooked()) != 1 {
		t.Fatalf("Sweep = %v, state %v, %d hooks; want a timeout that stands and is counted", err, r.state(t, "ix-1"), len(r.hooked()))
	}
}

// TestAnInteractionWithAPressWaitingIsNotTimedOut: a press recorded by the deadline may not be
// decided yet. Cancelling the task under it would drop a timely answer, so the sweep leaves the
// interaction until the press is decided, and does not count it against the batch.
func TestAnInteractionWithAPressWaitingIsNotTimedOut(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.pending(t, "ix-2", "task-2", t0.Add(timeout+time.Second))
	r.press.waiting["ix-1"] = true
	sw := r.sweeper(t, func(c *interact.SweepConfig) { c.Batch = 1 })
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.state(t, "ix-1") != store.InteractionPending || r.state(t, "ix-2") != store.InteractionTimedOut {
		t.Fatal("the interaction with a press waiting must stay pending, and the next one must not be held back by it")
	}
	if got := r.sess.cancelled(); len(got) != 1 || got[0] != "task-2" || len(r.upd.list()) != 1 || len(r.notices(t)) != 1 {
		t.Errorf("cancels %v, %d edits, %d notices; want only ix-2's", got, len(r.upd.list()), len(r.notices(t)))
	}
	if want := "ix-1 " + t0.Add(timeout).Format(time.RFC3339); r.press.asked[0] != want {
		t.Errorf("asked %q; want the question for presses by the deadline: %q", r.press.asked[0], want)
	}
	r.press.waiting["ix-1"] = false // the press is decided
	if err := sw.Sweep(t.Context()); err != nil || r.state(t, "ix-1") != store.InteractionTimedOut {
		t.Fatalf("Sweep after the press was decided = %v, state %v; want it timed out", err, r.state(t, "ix-1"))
	}
}

// TestAPressCheckThatFailsHoldsTheTimeout: if it cannot be known, the task is not cancelled.
func TestAPressCheckThatFailsHoldsTheTimeout(t *testing.T) {
	r := newSweepRig(t)
	r.pending(t, "ix-1", "task-1", t0.Add(timeout))
	r.press.err = errBoom
	if err := r.sweeper(t, nil).Sweep(t.Context()); err == nil || r.state(t, "ix-1") != store.InteractionPending || len(r.sess.cancelled()) != 0 {
		t.Fatalf("Sweep = %v, state %v, cancels %v; want an error, and nothing done", err, r.state(t, "ix-1"), r.sess.cancelled())
	}
}
