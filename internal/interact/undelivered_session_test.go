package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// These tests run the real session manager, the intake with its tap, the sweeper and the outbox over one store, against an
// agent that is a script. What the sweep learns of a denial is what Session.Answer says, and the manager says "taken" before it
// sends: the sweep's fakes cannot show what the real manager does when the send fails.

// scriptedAgent is the remote agent: it pauses on an approval, takes or refuses an answer as scripted, and records its calls.
// A restart of Agent Kourier does not restart it, so one agent is shared by the processes of a test.
type scriptedAgent struct {
	mu       sync.Mutex
	state    a2a.TaskState
	answers  []*a2a.Message
	cancels  []a2a.TaskID
	answerAt func(n int) error    // the error of the n-th answer (from 0), nil to take it
	cancelAt func(n int) error    // the error of the n-th cancel
	gate     chan struct{}        // if set, an answer waits for it to be closed
	sendAt   func(n int) a2a.Task // the task the n-th send ends on (from 0); nil: the approval
	sends    int
}

func newScriptedAgent() *scriptedAgent { return &scriptedAgent{state: a2a.TaskStateInputRequired} }

func (a *scriptedAgent) paused() a2a.Task {
	task := approval()
	return task
}

func (a *scriptedAgent) emit(h agent.Handler, task a2a.Task) {
	h(agent.StateEvent{TaskID: task.ID, ContextID: task.ContextID, State: task.Status.State})
	h(agent.FinalEvent{Task: &task})
}

func (a *scriptedAgent) Send(_ context.Context, _ *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	n, at := a.sends, a.sendAt
	a.sends++
	a.mu.Unlock()
	task := a.paused()
	if at != nil {
		task = at(n)
	}
	a.emit(h, task)
	return nil
}

func (a *scriptedAgent) Follow(_ context.Context, id a2a.TaskID, h agent.Handler) error {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	task := a.paused()
	task.ID = id
	task.Status.State = state
	a.emit(h, task)
	return nil
}

func (a *scriptedAgent) GetTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &a2a.Task{ID: id, ContextID: "ctx-1", Status: a2a.TaskStatus{State: a.state, Timestamp: &t0}}, nil
}

func (a *scriptedAgent) CancelTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.cancels)
	a.cancels = append(a.cancels, id)
	if a.cancelAt != nil {
		if err := a.cancelAt(n); err != nil {
			return nil, err
		}
	}
	a.state = a2a.TaskStateCanceled
	return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

func (a *scriptedAgent) Answer(_ context.Context, msg *a2a.Message, h agent.Handler) error {
	a.mu.Lock()
	n := len(a.answers)
	a.answers = append(a.answers, msg)
	gate, at := a.gate, a.answerAt
	a.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if at != nil {
		if err := at(n); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.state = a2a.TaskStateCompleted
	a.mu.Unlock()
	task := a2a.Task{ID: msg.TaskID, ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: &t0}}
	a.emit(h, task)
	return nil
}

func (*scriptedAgent) FindTask(context.Context, string, string) (*a2a.Task, []a2a.TaskID, error) {
	return nil, nil, agent.ErrLookupUnsupported
}

func (a *scriptedAgent) answered() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.answers) }
func (a *scriptedAgent) sent() int     { a.mu.Lock(); defer a.mu.Unlock(); return a.sends }
func (a *scriptedAgent) cancelled() []a2a.TaskID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.cancels)
}

// down is an answer error the session takes for a send that never reached the agent.
var down = func(int) error { return fmt.Errorf("%w: down", agent.ErrNotSent) }

// clockAt is the clock of a test, which it moves.
type clockAt struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockAt) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clockAt) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// recSink is the session's sink: it renders nothing and keeps the notices and the calls the manager audits.
type recSink struct {
	mu      sync.Mutex
	notices []string // "kind id detail"
}

func (s *recSink) Turn(context.Context, session.TurnRef) agent.Handler { return nil }

func (s *recSink) Notice(_ context.Context, _ session.Key, n session.Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices = append(s.notices, fmt.Sprintf("%s %s %s", n.Kind, n.ID, n.Detail))
}

func (s *recSink) seen() []string { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.notices) }

// callAudit is the manager's auditor: the calls it makes to the agent, by op.
type callAudit struct {
	mu  sync.Mutex
	ops []string
}

func (c *callAudit) AgentCall(_ context.Context, a audit.Actor, call audit.Call) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, fmt.Sprintf("%s by %q ok=%v", call.Op, a.User, call.Err == nil))
	return nil
}

func (c *callAudit) seen() []string { c.mu.Lock(); defer c.mu.Unlock(); return slices.Clone(c.ops) }

type oneBackend struct{ ag session.AgentAPI }

func (o oneBackend) Backend(string) (session.Backend, error) {
	return session.Backend{ThreadTTL: time.Hour, Agent: o.ag}, nil
}

type userIDs struct{}

func (userIDs) ResolveUser(_ context.Context, id string) (dialect.User, error) {
	return dialect.User{ID: id}, nil
}

// lateMgr is the manager as the intake knows it: bound once the manager is built, as the broker's is.
type lateMgr struct{ m *session.Manager }

func (l *lateMgr) Answer(ctx context.Context, k store.ConversationKey, msg *a2a.Message) error {
	return l.m.Answer(ctx, k, msg)
}

func (l *lateMgr) Cancel(ctx context.Context, k store.ConversationKey, id a2a.TaskID, reason string) error {
	return l.m.Cancel(ctx, k, id, reason)
}

// process is one run of Agent Kourier over a store: the manager, the intake behind its tap, the sweeper, and the fakes they
// write to. A restart is another process over the same store.
type process struct {
	t      *testing.T
	st     *sqlite.Store
	ag     *scriptedAgent
	clk    *clockAt
	mgr    *session.Manager
	tap    *interact.Tap
	sw     *interact.Sweeper
	re     *interact.Reopened
	sink   *recSink
	calls  *callAudit
	aud    *audits
	slack  *poster
	outsMu sync.Mutex
	outs   []string
}

// start runs a process. watch false leaves the manager with no watcher, which is how it behaved before one existed.
func start(t *testing.T, st *sqlite.Store, ag *scriptedAgent, clk *clockAt, watch bool) *process {
	t.Helper()
	p := &process{t: t, st: st, ag: ag, clk: clk, re: interact.NewReopened(st), sink: &recSink{}, calls: &callAudit{}, aud: &audits{}, slack: &poster{posted: map[string]string{}}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ob := newOutbox(t, outbox.Config{Store: st, Platform: p.slack, Now: clk.now, CallTimeout: 30 * time.Second, Logger: quiet})
	b := interact.Binding{Dialect: kagentv1.New(0), Timeout: timeout, ToolApprovals: true}
	count := func(binding string, o interact.Outcome) {
		p.outsMu.Lock()
		p.outs = append(p.outs, binding+" "+string(o))
		p.outsMu.Unlock()
	}
	late := &lateMgr{}
	in, err := interact.New(interact.Config{Store: st, AuditReader: st, Lookup: lookup{b}, Platform: p.slack, Outbox: ob, Session: late, Audit: p.aud, OnApprovalOutcome: count, Now: clk.now, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if p.tap, err = interact.NewTap(interact.TapConfig{Sink: p.sink, Handler: in, Lookup: lookup{b}, Backoff: 5 * time.Millisecond, Now: clk.now, Logger: quiet}); err != nil {
		t.Fatal(err)
	}
	cfg := session.Config{Context: t.Context(), Store: st, Questions: st, Bindings: oneBackend{ag}, Users: userIDs{}, Sink: p.tap, Now: func() time.Time { return t0 },
		TextReply: session.NoTextReply, ThreadContext: session.NoThreadContext, OnTurnEnd: func(string, session.TurnOutcome) {}, Audit: p.calls, Logger: quiet,
		RetryBase: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond}
	if watch {
		cfg.AnswerWatcher = p.re
	}
	if p.mgr, err = session.New(cfg); err != nil {
		t.Fatal(err)
	}
	late.m = p.mgr
	upd := &updates{}
	if p.sw, err = interact.NewSweeper(interact.SweepConfig{Store: st, Sessions: st, Lookup: lookup{b}, Session: p.mgr, Updater: upd, Outbox: ob, Auditor: p.aud, Presses: st,
		Reopened: p.re, Now: clk.now, Logger: quiet, OnTimeout: func(string) {}, OnApprovalOutcome: count}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	return p
}

// stop ends the process: the tap, then the manager.
func (p *process) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.tap.Close(ctx)
	_ = p.mgr.Close(ctx)
}

func (p *process) outcomes() []string {
	p.outsMu.Lock()
	defer p.outsMu.Unlock()
	return slices.Clone(p.outs)
}

// submit starts the turn whose task pauses on the approval, and waits for its interaction to be recorded and posted.
func (p *process) submit() store.Interaction {
	p.t.Helper()
	if _, err := p.mgr.Submit(p.t.Context(), session.Inbound{Key: key, Binding: "ns/b", MessageID: "slack:main:C1:1.2", MessageRef: "1.2", UserID: "U1", Text: "hello", Create: true}); err != nil {
		p.t.Fatal(err)
	}
	return p.waitRow(func(row store.Interaction) bool { return row.MessageRef != "" })
}

// waitRow waits for the interaction of the approval to be as ok says.
func (p *process) waitRow(ok func(store.Interaction) bool) store.Interaction {
	p.t.Helper()
	in, err := (&kagentv1.Dialect{}).ParseInteraction(approval())
	if err != nil || in == nil {
		p.t.Fatal(in, err)
	}
	var row store.Interaction
	until(p.t, "the interaction", func() bool {
		var err error
		row, err = p.st.GetInteraction(p.t.Context(), interact.InteractionID(*in))
		return err == nil && ok(row)
	})
	return row
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sweepsUntil sweeps, as the loop does, until cond holds, and returns the errors the sweeps gave.
func (p *process) sweepsUntil(what string, cond func() bool) (errs []error) {
	p.t.Helper()
	until(p.t, what, func() bool {
		if err := p.sw.Sweep(p.t.Context()); err != nil {
			errs = append(errs, err)
		}
		return cond()
	})
	return errs
}

// sweeps sweeps n times, as quiet sweeps that find nothing.
func (p *process) sweeps(n int) {
	p.t.Helper()
	for range n {
		if err := p.sw.Sweep(p.t.Context()); err != nil {
			p.t.Fatal(err)
		}
	}
}

func (p *process) notices() []string {
	p.t.Helper()
	rows, err := p.st.ListUnposted(p.t.Context())
	if err != nil {
		p.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Payload)
	}
	return out
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// pastDeadline is a time at which the approval of a pause made at t0 is due and past its grace.
func pastDeadline() time.Time { return t0.Add(timeout + time.Minute) }

func atTimes(clk *clockAt, at time.Time) *clockAt { clk.set(at); return clk }

func requireNotices(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("notices %q; want %d: %q", got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("notice %d is %q; want it to say %q", i, got[i], want[i])
		}
	}
}

// TestADenialTheAgentRefusesToTakeEndsWithTheTaskCancelled is the limit this removes: the session takes the timeout's denial and
// then cannot send it. The task is cancelled, and the thread told, without a restart; the row stays timed out, the denial is not
// sent again, and the timeout is audited and counted once. The session's own notice, worded for a person's answer, is not said.
func TestADenialTheAgentRefusesToTakeEndsWithTheTaskCancelled(t *testing.T) {
	ag := newScriptedAgent()
	ag.answerAt = down
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	row := p.submit()
	p.clk.set(pastDeadline())

	errs := p.sweepsUntil("the task to be cancelled", func() bool { return len(ag.cancelled()) >= 1 })
	if len(errs) != 0 {
		t.Errorf("sweeps failed: %v", errs)
	}
	p.sweeps(5)

	if got := ag.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("the agent was asked to cancel %v; want the task, once", got)
	}
	if got := ag.answered(); got != 1 {
		t.Errorf("the agent was sent %d answers; want the one denial: nothing is denied again", got)
	}
	if got := p.ag.answers[0]; got.ID != row.ID+"/answer" || got.TaskID != "task-1" {
		t.Errorf("answer %q for %q; want the interaction's, for task-1", got.ID, got.TaskID)
	}
	requireNotices(t, p.notices(), deniedNotice, undeliveredNotice)
	if got := p.sink.seen(); len(got) != 0 {
		t.Errorf("the session said %q; want nothing: its notice is worded for a person's answer, and the sweeper tells the thread", got)
	}
	if now, err := p.st.GetInteraction(t.Context(), row.ID); err != nil || now.State != store.InteractionTimedOut {
		t.Errorf("row %+v, %v; want it left timed out", now, err)
	}
	if len(p.aud.got) != 1 || p.aud.got[0].Result != audit.DecisionTimedOut || !slices.Equal(p.outcomes(), []string{"ns/b timed_out"}) {
		t.Errorf("audit %+v, counted %q; want the timeout audited and counted once, and no row for the cancel", p.aud.got, p.outcomes())
	}
	// The cancel is audited by the session as any is: an agent call, naming no person.
	if cancels := countOf(p.calls.seen(), "cancel by \"\" ok=true"); cancels != 1 {
		t.Errorf("agent calls audited %q; want one cancel by no person", p.calls.seen())
	}
	until(t, "the session to clear the task", func() bool { return !p.mgr.HoldsTask(key, "task-1") })
}

func countOf(all []string, one string) int {
	n := 0
	for _, s := range all {
		if s == one {
			n++
		}
	}
	return n
}

// TestADenialTheAgentTookIsNotFollowedByACancel: the same, with the send working. The agent has the denial, the task goes on,
// and no sweep cancels it, however many follow.
func TestADenialTheAgentTookIsNotFollowedByACancel(t *testing.T) {
	ag := newScriptedAgent()
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	p.submit()
	p.clk.set(pastDeadline())
	p.sweepsUntil("the denial to reach the agent", func() bool { return ag.answered() == 1 })
	until(t, "the task to end", func() bool { return !p.mgr.HoldsTask(key, "task-1") })
	p.sweeps(5)
	if got := ag.cancelled(); len(got) != 0 {
		t.Errorf("the agent was asked to cancel %v after it took the denial; want nothing", got)
	}
	requireNotices(t, p.notices(), deniedNotice)
	for _, n := range p.sink.seen() {
		if strings.HasPrefix(n, string(session.NoticeFailed)) {
			t.Errorf("the session said %q of a denial that was delivered", n)
		}
	}
	if len(p.aud.got) != 1 || !slices.Equal(p.outcomes(), []string{"ns/b timed_out"}) || ag.answered() != 1 {
		t.Errorf("audit %+v, counted %q, %d answers; want one timeout audited and counted, and the one denial", p.aud.got, p.outcomes(), ag.answered())
	}
}

// TestACancelThatFailsAfterADenialThatFailedIsRetried: the agent refuses the cancel at first. The sweeper asks again each sweep
// (the session retries within one), the thread is told once, and the denial is not sent again.
func TestACancelThatFailsAfterADenialThatFailedIsRetried(t *testing.T) {
	ag := newScriptedAgent()
	ag.answerAt = down
	ag.cancelAt = func(n int) error { // the first Cancel makes three attempts, and all fail
		if n < 3 {
			return errors.New("the agent is unreachable")
		}
		return nil
	}
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	p.submit()
	p.clk.set(pastDeadline())
	errs := p.sweepsUntil("the cancel to work", func() bool { return len(ag.cancelled()) >= 4 })
	if len(errs) == 0 {
		t.Error("no sweep failed although the cancel did")
	}
	p.sweeps(5)
	if got := len(ag.cancelled()); got != 4 || ag.answered() != 1 {
		t.Errorf("%d cancels, %d answers; want three attempts that failed and one that worked, and one denial", got, ag.answered())
	}
	requireNotices(t, p.notices(), deniedNotice, undeliveredNotice)
	if p.sw.UndeliveredCount() != 0 {
		t.Error("the sweeper still holds a denial it dealt with")
	}
}

// TestAPersonStoppingTheTaskBeforeTheCancelLeavesNothingMoreToDo: the pause ended some other way between the failed send and the
// sweep. The cancel names the task and finds it not paused, so the agent is not asked again and the thread is told nothing.
func TestAPersonStoppingTheTaskBeforeTheCancelLeavesNothingMoreToDo(t *testing.T) {
	ag := newScriptedAgent()
	ag.answerAt = down
	ag.gate = make(chan struct{})
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	p.submit()
	p.clk.set(pastDeadline())
	p.sweepsUntil("the denial to be sent", func() bool { return ag.answered() == 1 })
	close(ag.gate) // the send fails now
	until(t, "the session to tell the watcher", func() bool { return p.re.Len() == 1 })
	if did, err := p.mgr.Stop(t.Context(), key, "task-1", "U9"); err != nil || !did {
		t.Fatalf("Stop = %v, %v", did, err)
	}
	p.sweeps(4)
	if got := ag.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) { // the one Stop made
		t.Errorf("the agent was asked to cancel %v; want only the stop's one", got)
	}
	requireNotices(t, p.notices(), deniedNotice)
}

// TestADenialThatFailsAcrossARestartIsDeniedOnceMoreAndThenCancelled: the first process takes the denial, the send fails, and it
// dies before the sweep that would cancel. The pause is found again by the next, which denies it once more (the row is timed
// out, the Binding allows approvals) and, that failing too, cancels the task. Nothing is stored for it, and it is never denied
// for ever.
func TestADenialThatFailsAcrossARestartIsDeniedOnceMoreAndThenCancelled(t *testing.T) {
	st, ag := openStore(t), newScriptedAgent()
	ag.answerAt = down
	ag.gate = make(chan struct{}) // the send fails only when the test lets it, with no sweep to see it
	clk := atTimes(&clockAt{}, t0.Add(time.Minute))
	first := start(t, st, ag, clk, true)
	first.submit()
	clk.set(pastDeadline())
	first.sweepsUntil("the denial to be sent", func() bool { return ag.answered() == 1 })
	close(ag.gate)
	until(t, "the watcher to be told", func() bool { return first.re.Len() == 1 })
	first.stop() // the process dies with the cancel not made
	if got := ag.cancelled(); len(got) != 0 {
		t.Fatalf("cancelled %v before the restart", got)
	}

	second := start(t, st, ag, clk, true)
	if err := second.mgr.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	until(t, "the denial to be sent again", func() bool { return ag.answered() == 2 })
	second.sweepsUntil("the task to be cancelled", func() bool { return len(ag.cancelled()) >= 1 })
	second.sweeps(5)

	if got := ag.answered(); got != 2 {
		t.Errorf("%d answers; want the first process's denial and one more", got)
	}
	if got := ag.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("cancelled %v; want the task, once", got)
	}
	requireNotices(t, second.notices(), deniedNotice, undeliveredNotice)
	if len(second.aud.got) != 1 || second.aud.got[0].Result != audit.DecisionTimedOut || len(second.outcomes()) != 0 || len(first.aud.got) != 1 || !slices.Equal(first.outcomes(), []string{"ns/b timed_out"}) {
		t.Errorf("audits %d and %d, counted %q and %q; want the timeout audited and counted once by the first process, with the recovery audit before its denial", len(first.aud.got), len(second.aud.got), first.outcomes(), second.outcomes())
	}
}

// TestARecoveredTimeoutRetryOutlivesTheOriginalDeadline: recovery finds the timed-out approval after its
// original deadline, but its first recovery audit fails. Once the audit is available again, the tap retries
// the denial without requiring another Manager.Recover call.
func TestARecoveredTimeoutRetryOutlivesTheOriginalDeadline(t *testing.T) {
	st, ag := openStore(t), newScriptedAgent()
	ag.answerAt = func(n int) error {
		if n == 0 {
			return down(n)
		}
		return nil
	}
	ag.gate = make(chan struct{})
	clk := atTimes(&clockAt{}, t0.Add(time.Minute))
	first := start(t, st, ag, clk, true)
	first.submit()
	clk.set(pastDeadline())
	first.sweepsUntil("the initial denial to be sent", func() bool { return ag.answered() == 1 })
	close(ag.gate)
	until(t, "the watcher to be told", func() bool { return first.re.Len() == 1 })
	first.stop()

	second := start(t, st, ag, clk, true)
	second.aud.setError(errBoom)
	if err := second.mgr.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	until(t, "the recovery audit to fail", func() bool { return second.aud.decisionCount() == 1 })
	second.aud.setError(nil)
	until(t, "the recovered denial to be retried", func() bool { return ag.answered() == 2 })
	second.sweeps(5)

	if got := ag.cancelled(); len(got) != 0 {
		t.Errorf("cancelled %v; want no cancel after the recovered denial is delivered", got)
	}
	if got := second.aud.decisionCount(); got != 2 {
		t.Errorf("recovery wrote %d timeout audits; want the failed attempt and its successful retry", got)
	}
}

// TestAPersonsAnswerThatFailsToSendBehavesAsItDidBeforeTheWatcher is a golden comparison: the same failure of a person's
// answer, once with the watcher wired and once with no watcher (the manager as it was), leaves the same thread, the same agent
// calls and audit rows, the same open pause, and no cancel by any sweep.
func TestAPersonsAnswerThatFailsToSendBehavesAsItDidBeforeTheWatcher(t *testing.T) {
	run := func(watch bool) []string {
		ag := newScriptedAgent()
		ag.answerAt = func(n int) error {
			if n == 0 {
				return fmt.Errorf("%w: down", agent.ErrNotSent)
			}
			return nil
		}
		p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), watch)
		row := p.submit()
		var in dialect.Interaction
		if err := json.Unmarshal(row.Request, &in); err != nil {
			t.Fatal(err)
		}
		answer := dialect.Answer{RequestID: in.RequestID, Decisions: []dialect.Decision{{ToolID: "t1", Approved: true}, {ToolID: "t2", Approved: false}}}
		raw, _ := json.Marshal(answer)
		if err := p.st.SubmitInteraction(t.Context(), row.ID, raw, "UALICE1", t0.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		msg, err := interact.AnswerMessage(kagentv1.New(0), row.ID, in, answer, "UALICE1", dialect.Platform{})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.mgr.Answer(t.Context(), key, &msg); err != nil {
			t.Fatal(err)
		}
		until(t, "the failure to be told", func() bool { return len(p.sink.seen()) == 1 })
		p.clk.set(pastDeadline())
		p.sweeps(3)
		got := []string{fmt.Sprintf("cancels %v", ag.cancelled()), fmt.Sprintf("answers %d", ag.answered()), fmt.Sprintf("held %d", p.re.Len())}
		// The pause is still open: it takes the person's answer again, as it always did.
		if err := p.mgr.Answer(t.Context(), key, answerMsgFor(t, row, in, answer)); err != nil {
			t.Errorf("a second answer = %v; want the pause open", err)
		}
		// Answer returns once the runner has the answer; the task it completes ends on the runner. Both runs are compared
		// once that turn is over: it has told the thread there was no output, and the runner has let go of the task.
		until(t, "the second answer's turn to end", func() bool {
			return slices.ContainsFunc(p.sink.seen(), func(n string) bool { return strings.HasPrefix(n, string(session.NoticeNoOutput)+" ") }) &&
				!p.mgr.HoldsTask(key, "task-1")
		})
		got = append(got, p.sink.seen()...)
		got = append(got, p.calls.seen()...)
		got = append(got, p.notices()...)
		return got
	}
	with, without := run(true), run(false)
	if !slices.Equal(with, without) {
		t.Errorf("with the watcher %q\nwithout it    %q; want the same", with, without)
	}
	if len(with) < 4 || !strings.Contains(with[3], "failed ") || !strings.Contains(with[3], "the answer could not be delivered") {
		t.Errorf("record %q; want the session's own notice that the answer could not be delivered", with)
	}
	if with[0] != "cancels []" {
		t.Errorf("record %q; want no cancel", with)
	}
}

func answerMsgFor(t *testing.T, row store.Interaction, in dialect.Interaction, answer dialect.Answer) *a2a.Message {
	t.Helper()
	msg, err := interact.AnswerMessage(kagentv1.New(0), row.ID, in, answer, "UALICE1", dialect.Platform{})
	if err != nil {
		t.Fatal(err)
	}
	return &msg
}
