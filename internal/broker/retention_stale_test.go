package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// The retention loop ends a task that a crash left on a session past its horizon (data protection, DP-4): the agent is asked
// to cancel it, bounded and best effort, and then the store clears it, cancels its pending interactions and the loop audits
// it, so that the purge of sessions is not held back by it for ever.

func (f *fakeRetentionStore) ListStaleTasks(_ context.Context, h store.SessionHorizons, limit int) ([]store.StaleTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "ListStaleTasks")
	f.staleLists = append(f.staleLists, sessionPurge{h, limit})
	if f.failList {
		return nil, errors.New("disk full")
	}
	return append([]store.StaleTask(nil), f.staleQueue[:min(limit, len(f.staleQueue))]...), nil
}

func (f *fakeRetentionStore) EndStaleTask(_ context.Context, _ store.SessionHorizons, t store.StaleTask, _ time.Time) (store.EndedStaleTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "EndStaleTask")
	f.endCalls = append(f.endCalls, t)
	if err := f.endErr[t.TaskID]; err != nil {
		return store.EndedStaleTask{}, err
	}
	got, ok := f.endAs[t.TaskID]
	if !ok {
		got = store.EndedStaleTask{Ended: true}
	}
	if got.Ended {
		f.staleQueue = slicesDeleteFunc(f.staleQueue, func(q store.StaleTask) bool { return q.Key == t.Key })
	}
	return got, nil
}

func slicesDeleteFunc(s []store.StaleTask, del func(store.StaleTask) bool) []store.StaleTask {
	var out []store.StaleTask
	for _, t := range s {
		if !del(t) {
			out = append(out, t)
		}
	}
	return out
}

// fakeCanceller is the agents: it records each cancel, and answers by Binding. A Binding in block never answers, as an agent
// that is down in a way that holds the connection; a hook, if set, runs first.
type fakeCanceller struct {
	f     *fakeRetentionStore // its order is shared, so a test sees the cancel come before the end
	mu    sync.Mutex
	calls []cancelCall
	errs  map[string]error // by Binding
	block map[string]bool
	hook  func()
}

type cancelCall struct{ binding, task string }

// CancelTask answers with the Binding's identity, "sa/<Binding>", as Bindings does once it has resolved the Binding, which it
// has not for one that has left the config.
func (c *fakeCanceller) CancelTask(ctx context.Context, binding, taskID string) (string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, cancelCall{binding, taskID})
	c.mu.Unlock()
	if c.f != nil {
		c.f.mu.Lock()
		c.f.order = append(c.f.order, "CancelTask")
		c.f.mu.Unlock()
	}
	if c.hook != nil {
		c.hook()
	}
	if c.block[binding] {
		<-ctx.Done()
		return "sa/" + binding, ctx.Err()
	}
	if errors.Is(c.errs[binding], session.ErrUnknownBinding) {
		return "", c.errs[binding]
	}
	return "sa/" + binding, c.errs[binding]
}

func (c *fakeCanceller) got() []cancelCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cancelCall(nil), c.calls...)
}

// fakeStaleAudit records what is audited.
type fakeStaleAudit struct {
	mu   sync.Mutex
	rows []auditedStale
	err  error
}

type auditedStale struct {
	actor audit.Actor
	task  audit.StaleTask
}

func (a *fakeStaleAudit) StaleTaskEnded(_ context.Context, actor audit.Actor, t audit.StaleTask) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, auditedStale{actor, t})
	return a.err
}

func (a *fakeStaleAudit) got() []auditedStale {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]auditedStale(nil), a.rows...)
}

func stale(thread, binding, task string) store.StaleTask {
	return store.StaleTask{Key: store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}, Binding: binding, ContextID: "ctx-" + thread, TaskID: task}
}

// staleRig is a retention over fakes, and what it counted.
type staleRig struct {
	f      *fakeRetentionStore
	agents *fakeCanceller
	audit  *fakeStaleAudit
	r      *retention
	mu     sync.Mutex
	count  map[string]int
	logs   *logBuffer
}

func newStaleRig(t *testing.T, queue []store.StaleTask, mutate func(*retentionConfig)) *staleRig {
	t.Helper()
	f := &fakeRetentionStore{staleQueue: queue}
	rig := &staleRig{f: f, agents: &fakeCanceller{f: f}, audit: &fakeStaleAudit{}, count: map[string]int{}, logs: &logBuffer{}}
	cfg := retentionConfig{Store: f, ThreadTTLs: ttls, Canceller: rig.agents, Audit: rig.audit, Holds: func(store.ConversationKey, string) bool { return false }, StaleBatch: 3, Now: func() time.Time { return sweepNow },
		OnRemoved: func(what string, n int) { rig.mu.Lock(); rig.count[what] += n; rig.mu.Unlock() },
		Logger:    slog.New(slog.NewTextHandler(rig.logs, nil))}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := newRetention(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rig.r = r
	return rig
}

func (g *staleRig) counted() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.count)
}

// The loop asks for the sessions past each Binding's horizon (the one DP-3 deletes by), cancels each task at its agent
// before it ends it, ends it, audits it by identifiers, and counts the tasks and the interactions that went with them.
func TestRetentionEndsAStaleTaskAfterCancellingItAtItsAgent(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/long", "t1"), stale("2.2", "ns/short", "t2")}, nil)
	g.f.endAs = map[string]store.EndedStaleTask{"t1": {Ended: true, Interactions: 2}}
	g.r.endStaleTasks(t.Context())

	day, grace := 24*time.Hour, 90*24*time.Hour
	wantH := store.SessionHorizons{
		ByBinding: map[string]time.Time{"ns/long": sweepNow.Add(-30*day - grace), "ns/short": sweepNow.Add(-10*day - grace)},
		Otherwise: sweepNow.Add(-90*day - grace),
	}
	if len(g.f.staleLists) == 0 || !maps.EqualFunc(g.f.staleLists[0].horizons.ByBinding, wantH.ByBinding, time.Time.Equal) || !g.f.staleLists[0].horizons.Otherwise.Equal(wantH.Otherwise) || g.f.staleLists[0].limit != 3 {
		t.Errorf("ListStaleTasks calls = %+v; want the horizons of DP-3 (%+v) and the batch of 3", g.f.staleLists, wantH)
	}
	if got, want := g.agents.got(), []cancelCall{{"ns/long", "t1"}, {"ns/short", "t2"}}; !equal(got, want) {
		t.Errorf("cancels = %+v; want %+v", got, want)
	}
	if got, want := g.f.order, []string{"ListStaleTasks", "CancelTask", "EndStaleTask", "CancelTask", "EndStaleTask"}; !equal(got, want) {
		t.Errorf("calls in order = %v; want each cancel before its end, after the one listing", got)
	}
	want := []auditedStale{
		{audit.Actor{Binding: "ns/long", Identity: "sa/ns/long"}, audit.StaleTask{TaskID: "t1", ContextID: "ctx-1.1", Cancel: audit.StaleCancelOK, Interactions: 2}},
		{audit.Actor{Binding: "ns/short", Identity: "sa/ns/short"}, audit.StaleTask{TaskID: "t2", ContextID: "ctx-2.2", Cancel: audit.StaleCancelOK}},
	}
	if got := g.audit.got(); !equal(got, want) {
		t.Errorf("audited %+v; want %+v: identifiers only", got, want)
	}
	if got := g.counted(); !maps.Equal(got, map[string]int{RemovedStaleTasks: 2, RemovedStaleInteractions: 2}) {
		t.Errorf("counted %v; want 2 tasks and the 2 interactions", got)
	}
	if got := g.logs.String(); !strings.Contains(got, "ended tasks that a crash left") || strings.Contains(got, "t1") || strings.Contains(got, "ctx-") {
		t.Errorf("the log %q; want one line of counts, naming no task or context", got)
	}
}

func TestRetentionClearsStaleAcknowledgmentAfterEndingTheTask(t *testing.T) {
	task := stale("1.1", "ns/long", "t1")
	g := newStaleRig(t, []store.StaleTask{task}, nil)
	g.f.staleTurns = map[string]store.TaskTurn{"t1": {ID: "question/answer", AcknowledgmentID: "message-1", MessageRef: "1.1", Acknowledgment: "eyes"}}
	var cleared []store.Reply
	g.r.cfg.ClearAcknowledgment = func(_ context.Context, r store.Reply) error {
		g.f.mu.Lock()
		defer g.f.mu.Unlock()
		cleared = append(cleared, r)
		if len(g.f.order) == 0 || g.f.order[len(g.f.order)-1] != "EndStaleTask" {
			t.Errorf("acknowledgment cleared before stale task was ended: %v", g.f.order)
		}
		return nil
	}
	g.r.endStaleTasks(t.Context())
	want := []store.Reply{{ID: "message-1", Key: task.Key, MessageRef: "1.1", Acknowledgment: "eyes"}}
	if !equal(cleared, want) {
		t.Errorf("cleared acknowledgments = %+v; want %+v", cleared, want)
	}
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An agent that cannot be reached does not stop the sweep: the task is ended anyway, the failure is audited as its type, and
// the agent is not asked again in the tick (each task would cost a timeout). An agent that answers that it does not know the
// task is up, and is asked for the next.
func TestRetentionEndsAStaleTaskWhoseAgentCannotBeReached(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{
		stale("1.1", "ns/short", "t1"), stale("2.2", "ns/short", "t2"), stale("3.3", "ns/long", "t3"), stale("4.4", "ns/gone-task", "t4"), stale("5.5", "ns/gone-task", "t5"),
	}, func(c *retentionConfig) { c.StaleBatch = 10 })
	g.agents.errs = map[string]error{"ns/short": errors.New("connection refused"), "ns/gone-task": fmt.Errorf("cancel: %w", a2a.ErrTaskNotFound)}
	g.r.endStaleTasks(t.Context())

	if got, want := g.agents.got(), []cancelCall{{"ns/short", "t1"}, {"ns/long", "t3"}, {"ns/gone-task", "t4"}, {"ns/gone-task", "t5"}}; !equal(got, want) {
		t.Errorf("cancels = %+v; want t2 not asked of the agent that did not answer, and both of the agent that knew no task", got)
	}
	var cancels []audit.StaleCancel
	for _, a := range g.audit.got() {
		cancels = append(cancels, a.task.Cancel)
		// The identity is the one the call was made under: of a call that was made, failed or not, and none for one that was not.
		if want := "sa/" + a.actor.Binding; (a.task.Cancel == audit.StaleCancelSkipped) != (a.actor.Identity == "") || (a.task.Cancel != audit.StaleCancelSkipped && a.actor.Identity != want) {
			t.Errorf("task %s (%s) audited under identity %q; want %q for a call made, and none for one skipped", a.task.TaskID, a.task.Cancel, a.actor.Identity, want)
		}
	}
	if want := []audit.StaleCancel{audit.StaleCancelFailed, audit.StaleCancelSkipped, audit.StaleCancelOK, audit.StaleCancelFailed, audit.StaleCancelFailed}; !equal(cancels, want) {
		t.Errorf("audited cancels = %v; want %v", cancels, want)
	}
	if rows := g.audit.got(); rows[0].task.CancelErr == nil || !errors.Is(rows[3].task.CancelErr, a2a.ErrTaskNotFound) {
		t.Errorf("audited errors = %v, %v; want the failures' own, for their types", rows[0].task.CancelErr, rows[3].task.CancelErr)
	}
	if len(g.f.endCalls) != 5 || g.counted()[RemovedStaleTasks] != 5 {
		t.Errorf("%d tasks ended, %d counted; want all 5, whatever the agents said", len(g.f.endCalls), g.counted()[RemovedStaleTasks])
	}
}

// The cancel is bounded: an agent that holds the connection and never answers costs one timeout, and the sweep ends the tasks.
func TestRetentionBoundsTheCancelOfAStaleTask(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1"), stale("2.2", "ns/short", "t2"), stale("3.3", "ns/long", "t3")},
		func(c *retentionConfig) { c.CancelTimeout = 20 * time.Millisecond })
	g.agents.block = map[string]bool{"ns/short": true}
	done := make(chan struct{})
	go func() { g.r.endStaleTasks(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep waited on an agent that never answers")
	}
	if got := g.agents.got(); len(got) != 2 || got[0].task != "t1" || got[1].task != "t3" {
		t.Errorf("cancels = %+v; want one timeout for the agent that held the connection, and the other agent's", got)
	}
	rows := g.audit.got()
	if len(rows) != 3 || !errors.Is(rows[0].task.CancelErr, context.DeadlineExceeded) || rows[0].task.Cancel != audit.StaleCancelFailed || rows[1].task.Cancel != audit.StaleCancelSkipped {
		t.Errorf("audited %+v; want the timeout as the first's failure, the second skipped, and all three ended", rows)
	}
}

// A Binding that has left the config has no agent to ask: the task is ended, and audited as such, and the other Bindings'
// tasks go on being cancelled.
func TestRetentionEndsAStaleTaskWhoseBindingHasLeft(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/removed", "t1"), stale("2.2", "ns/removed", "t2"), stale("3.3", "ns/short", "t3")}, nil)
	g.agents.errs = map[string]error{"ns/removed": fmt.Errorf("session Binding %q: %w", "ns/removed", session.ErrUnknownBinding)}
	g.r.endStaleTasks(t.Context())
	var cancels []audit.StaleCancel
	for _, a := range g.audit.got() {
		cancels = append(cancels, a.task.Cancel)
		if a.task.CancelErr != nil {
			t.Errorf("audited error %v for %s; want none for a Binding that has left", a.task.CancelErr, a.task.TaskID)
		}
		if (a.task.Cancel == audit.StaleCancelNoBinding) != (a.actor.Identity == "") {
			t.Errorf("task %s (%s) audited under identity %q; want none exactly when the Binding has left", a.task.TaskID, a.task.Cancel, a.actor.Identity)
		}
	}
	if want := []audit.StaleCancel{audit.StaleCancelNoBinding, audit.StaleCancelNoBinding, audit.StaleCancelOK}; !equal(cancels, want) {
		t.Errorf("audited cancels = %v; want %v", cancels, want)
	}
	if len(g.f.endCalls) != 3 {
		t.Errorf("%d tasks ended; want 3", len(g.f.endCalls))
	}
}

// A failure to audit does not undo the ending, and does not stop the rest.
func TestRetentionGoesOnWhenAnEndingCannotBeAudited(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1"), stale("2.2", "ns/short", "t2")}, nil)
	g.audit.err = errors.New("audit down")
	g.r.endStaleTasks(t.Context())
	if len(g.f.endCalls) != 2 || g.counted()[RemovedStaleTasks] != 2 {
		t.Errorf("%d ended, %d counted; want both", len(g.f.endCalls), g.counted()[RemovedStaleTasks])
	}
}

// One tick does not go on for ever: a listing at a time while one comes back full, at most MaxStaleBatches of them.
func TestRetentionBoundsTheStaleTasksOfOneTick(t *testing.T) {
	queue := func(n int) []store.StaleTask {
		var q []store.StaleTask
		for i := range n {
			q = append(q, stale(fmt.Sprintf("%d.1", i), "ns/short", fmt.Sprintf("t%d", i)))
		}
		return q
	}
	for _, tc := range []struct {
		name               string
		tasks, max         int
		wantLists, wantEnd int
	}{
		{"until a short listing", 7, 10, 3, 7},
		{"never short, so the most it may make", 100, 2, 2, 6},
		{"nothing due", 0, 10, 1, 0},
		{"a full listing that is the last asks again, and finds nothing", 6, 10, 3, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newStaleRig(t, queue(tc.tasks), func(c *retentionConfig) { c.MaxStaleBatches = tc.max })
			g.r.endStaleTasks(t.Context())
			if len(g.f.staleLists) != tc.wantLists || len(g.f.endCalls) != tc.wantEnd || len(g.agents.got()) != tc.wantEnd {
				t.Errorf("%d listings, %d ended, %d cancelled; want %d, %d, %d", len(g.f.staleLists), len(g.f.endCalls), len(g.agents.got()), tc.wantLists, tc.wantEnd, tc.wantEnd)
			}
		})
	}
}

// A session that is not ended (it moved on between the listing and the end, or its ending failed) is not counted or audited,
// and is not asked of the agent a second time in the tick, which also ends a tick whose listings find nothing new.
func TestRetentionDoesNotRepeatAStaleTaskItCouldNotEnd(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "moved"), stale("2.2", "ns/short", "broken"), stale("3.3", "ns/short", "fine"), stale("4.4", "ns/short", "later")},
		func(c *retentionConfig) { c.MaxStaleBatches = 20 })
	g.f.endAs = map[string]store.EndedStaleTask{"moved": {}}
	g.f.endErr = map[string]error{"broken": errors.New("lock timeout")}
	g.r.endStaleTasks(t.Context())

	tasks := func(cs []cancelCall) (out []string) {
		for _, c := range cs {
			out = append(out, c.task)
		}
		return out
	}
	if got := tasks(g.agents.got()); !equal(got, []string{"moved", "broken", "fine", "later"}) {
		t.Errorf("cancels = %v; want each task once", got)
	}
	if got := g.counted(); !maps.Equal(got, map[string]int{RemovedStaleTasks: 2}) {
		t.Errorf("counted %v; want the two that were ended", got)
	}
	if rows := g.audit.got(); len(rows) != 2 || rows[0].task.TaskID != "fine" || rows[1].task.TaskID != "later" {
		t.Errorf("audited %+v; want the two that were ended", rows)
	}
	if !strings.Contains(g.logs.String(), "a stale task could not be ended") {
		t.Errorf("the log %q; want a warning for the one that failed", g.logs.String())
	}
	if n := len(g.f.staleLists); n >= 20 {
		t.Errorf("%d listings; want the tick to stop when a listing holds nothing it has not tried", n)
	}
}

// A tick whose listings hold nothing it has not already tried ends: a full listing of sessions that all moved on would
// otherwise be asked for again, and each of its tasks cancelled again, until the tick's share was spent.
func TestRetentionStopsWhenNoStaleTaskCanBeEnded(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "a"), stale("2.2", "ns/short", "b"), stale("3.3", "ns/short", "c")},
		func(c *retentionConfig) { c.MaxStaleBatches = 20 })
	g.f.endAs = map[string]store.EndedStaleTask{"a": {}, "b": {}, "c": {}}
	g.r.endStaleTasks(t.Context())
	if len(g.f.staleLists) != 2 || len(g.agents.got()) != 3 || len(g.audit.got()) != 0 || len(g.counted()) != 0 {
		t.Errorf("%d listings, %d cancels, %d audits, counted %v; want the second listing to find nothing new and stop, and nothing ended",
			len(g.f.staleLists), len(g.agents.got()), len(g.audit.got()), g.counted())
	}
}

// A task that this process's session manager still runs (parked on a question it has not given up on, or streaming) is left
// to its runner: clearing it from under the runner would leave the runner parked on it after the session is deleted, and a
// new session under the same key would wait behind it. It is not cancelled at the agent, not ended, not audited or counted,
// and not asked of Holds again in the tick; the others go on, and the tick says it left one.
func TestRetentionLeavesAStaleTaskThatARunnerOfThisProcessHolds(t *testing.T) {
	held := stale("1.1", "ns/short", "parked")
	g := newStaleRig(t, []store.StaleTask{held, stale("2.2", "ns/short", "orphan")}, func(c *retentionConfig) { c.StaleBatch, c.MaxStaleBatches = 2, 20 }) // a full first listing, so that a second finds the held task again
	var asked []string
	g.r.cfg.Holds = func(k store.ConversationKey, task string) bool {
		asked = append(asked, task)
		return k == held.Key && task == "parked"
	}
	g.r.endStaleTasks(t.Context())
	if got := g.agents.got(); len(got) != 1 || got[0].task != "orphan" {
		t.Errorf("cancels = %+v; want only the task no runner holds", got)
	}
	if len(g.f.endCalls) != 1 || g.f.endCalls[0].TaskID != "orphan" || len(g.audit.got()) != 1 || g.counted()[RemovedStaleTasks] != 1 {
		t.Errorf("ends %+v, audits %d, counted %v; want the one that is not held", g.f.endCalls, len(g.audit.got()), g.counted())
	}
	if !equal(asked, []string{"parked", "orphan"}) {
		t.Errorf("Holds was asked of %v; want each task once", asked)
	}
	if !strings.Contains(g.logs.String(), "left stale tasks that this process still runs") {
		t.Errorf("the log %q; want a warning that one was left", g.logs.String())
	}
	// Once its runner has let go of it (the interaction timed out, or the process restarted), the task is ended.
	g.r.cfg.Holds = func(store.ConversationKey, string) bool { return false }
	g.r.endStaleTasks(t.Context())
	if len(g.f.endCalls) != 2 || g.f.endCalls[1].TaskID != "parked" {
		t.Errorf("ends %+v after the runner let go; want the parked task ended too", g.f.endCalls)
	}
}

// Every collaborator and hook of the retention is required, and the error names the field that is left out: Holds is what
// keeps a runner's task from being cleared from under it, and the rest are what the loop ends, cancels, records and counts
// with. A wiring that drops one fails every test that builds the broker.
func TestNewRetentionNamesTheFieldThatIsMissing(t *testing.T) {
	full := harmless(retentionConfig{Store: &fakeRetentionStore{}})
	for _, tc := range []struct {
		field string
		clear func(*retentionConfig)
	}{
		{"Store", func(c *retentionConfig) { c.Store = nil }},
		{"Holds", func(c *retentionConfig) { c.Holds = nil }},
		{"ThreadTTLs", func(c *retentionConfig) { c.ThreadTTLs = nil }},
		{"Canceller", func(c *retentionConfig) { c.Canceller = nil }},
		{"Audit", func(c *retentionConfig) { c.Audit = nil }},
		{"OnRemoved", func(c *retentionConfig) { c.OnRemoved = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			cfg := full
			tc.clear(&cfg)
			if r, err := newRetention(cfg); err == nil || r != nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("newRetention = %v, %v; want an error that names %s", r, err, tc.field)
			}
		})
	}
	if _, err := newRetention(full); err != nil {
		t.Errorf("newRetention with every field = %v; want none", err)
	}
}

// A listing that fails is logged and left for the next tick.
func TestRetentionLogsAStaleListingFailure(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1")}, nil)
	g.f.failList = true
	g.r.endStaleTasks(t.Context())
	if len(g.agents.got()) != 0 || len(g.f.endCalls) != 0 || !strings.Contains(g.logs.String(), "stale tasks could not be listed") {
		t.Errorf("cancels %v, ends %v, log %q; want nothing done, and a warning", g.agents.got(), g.f.endCalls, g.logs.String())
	}
}

// Without a config nothing is known of any session's threadTTL, so no task is judged stale and the loop does not look.
func TestRetentionLeavesTasksAloneUntilTheirBindingsAreKnown(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1")}, func(c *retentionConfig) {
		c.ThreadTTLs = func() (map[string]time.Duration, bool) { return nil, false }
	})
	g.r.endStaleTasks(t.Context())
	if len(g.f.staleLists) != 0 || len(g.agents.got()) != 0 {
		t.Errorf("%d listings, %d cancels; want none", len(g.f.staleLists), len(g.agents.got()))
	}
}

// A sweep that is stopped mid-cancel (the broker is shutting down) does not end the task: the cancel may not have been
// made, and the next run makes it.
func TestRetentionDoesNotEndATaskWhoseCancelWasInterrupted(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1"), stale("2.2", "ns/short", "t2")}, nil)
	ctx, stop := context.WithCancel(t.Context())
	g.agents.hook = stop
	g.r.endStaleTasks(ctx)
	if len(g.f.endCalls) != 0 || len(g.agents.got()) != 1 || len(g.audit.got()) != 0 {
		t.Errorf("ends %v, cancels %v, audits %v; want the first cancel only, and nothing ended", g.f.endCalls, g.agents.got(), g.audit.got())
	}
}

// Each tick ends the stale tasks first, so that what it frees (replies it holds, the session it holds) is dealt with by the
// passes that follow, in the same tick.
func TestRetentionEndsStaleTasksBeforeItDropsRepliesAndPurgesSessions(t *testing.T) {
	g := newStaleRig(t, []store.StaleTask{stale("1.1", "ns/short", "t1")}, nil)
	stop := runRetention(t, retentionConfig{Store: g.f, ThreadTTLs: ttls, Canceller: g.agents, Audit: g.audit, Holds: func(store.ConversationKey, string) bool { return false }})
	waitUntil(t, "a purge", func() bool { g.f.mu.Lock(); defer g.f.mu.Unlock(); return len(g.f.sessionCalls) > 0 })
	stop()
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if want := []string{"ListStaleTasks", "CancelTask", "EndStaleTask", "AbandonStrandedReplies", "PurgeExpiredSessions"}; len(g.f.order) < len(want) || !equal(g.f.order[:len(want)], want) {
		t.Errorf("calls of the first tick = %v; want %v", g.f.order, want)
	}
}

// Against the real store, through the loop: the task of a session past its horizon is cancelled at the agent, cleared, and
// audited, with its pending interaction cancelled, and the session is then deleted by the next purge. A session inside its
// horizon, a Binding's own horizon included, is left whole, its task and its question with it.
func TestRetentionEndsAStaleTaskAndTheNextPurgeDeletesTheSession(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	day, grace := 24*time.Hour, 90*24*time.Hour
	seed := func(thread, binding, task string, idle time.Duration) store.ConversationKey {
		t.Helper()
		k := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}
		if err := st.UpsertSession(ctx, store.Session{Key: k, Binding: binding, ContextID: "ctx-" + thread, LastActivity: sweepNow.Add(-idle)}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSessionTask(ctx, k, "", task, store.TaskTurn{ID: "turn", User: "U1"}); err != nil {
			t.Fatal(err)
		}
		_, err := st.CreateInteraction(ctx, store.Interaction{ID: "ask-" + thread, Key: k, TaskID: task, Kind: store.InteractionAskUser,
			Request: []byte(`{}`), CreatedAt: sweepNow.Add(-idle), Deadline: sweepNow.Add(-idle + time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	past := seed("1.1", "ns/short", "crashed", 10*day+grace+time.Hour)
	inside := seed("2.2", "ns/short", "running", 10*day+grace-time.Hour)
	longer := seed("3.3", "ns/long", "long", 10*day+grace+time.Hour) // past the short Binding's horizon, not its own
	left := seed("4.4", "ns/removed", "orphan", 90*day+grace+time.Hour)

	agents := &fakeCanceller{errs: map[string]error{"ns/removed": session.ErrUnknownBinding}}
	rig := newStaleRig(t, nil, func(c *retentionConfig) {
		w, err := audit.New(st, audit.Options{Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnFailure: audit.NoFailure})
		if err != nil {
			t.Fatal(err)
		}
		c.Store, c.Canceller, c.Audit = st, agents, w
		c.StaleBatch = 10
	})

	rig.r.endStaleTasks(ctx)
	if got, want := agents.got(), []cancelCall{{"ns/removed", "orphan"}, {"ns/short", "crashed"}}; !equal(got, want) {
		t.Errorf("cancels = %+v; want %+v, and none for a task inside its horizon", got, want)
	}
	for k, want := range map[store.ConversationKey]struct {
		task  string
		state store.InteractionState
	}{past: {"", store.InteractionCancelled}, left: {"", store.InteractionCancelled}, inside: {"running", store.InteractionPending}, longer: {"long", store.InteractionPending}} {
		s, err := st.GetSession(ctx, k, 100*365*day, sweepNow)
		if err != nil || s.TaskID != want.task {
			t.Errorf("session %s after the end = task %q, %v; want task %q, and still there for the purge", k.Thread, s.TaskID, err, want.task)
		}
		if i, err := st.GetInteraction(ctx, "ask-"+k.Thread); err != nil || i.State != want.state {
			t.Errorf("interaction of %s = %s, %v; want %s", k.Thread, i.State, err, want.state)
		}
	}
	if got := rig.counted(); !maps.Equal(got, map[string]int{RemovedStaleTasks: 2, RemovedStaleInteractions: 2}) {
		t.Errorf("counted %v; want 2 tasks and their 2 interactions", got)
	}
	entries, err := st.ListAudit(ctx, time.Time{}, 0, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit = %+v, %v; want one entry for each task that was ended", entries, err)
	}
	for i, want := range []struct{ subject, outcome, binding, identity string }{
		{"task=orphan context=ctx-4.4", "stale_task_ended: cancel=no_binding interactions=1", "ns/removed", ""}, // no call, so no identity
		{"task=crashed context=ctx-1.1", "stale_task_ended: cancel=ok interactions=1", "ns/short", "sa/ns/short"},
	} {
		if e := entries[i]; e.Kind != store.AuditEvent || e.Subject != want.subject || e.Outcome != want.outcome || e.Binding != want.binding || e.Identity != want.identity {
			t.Errorf("audit entry %d = %+v; want an event %q, %q of %s as %q", i, e, want.subject, want.outcome, want.binding, want.identity)
		}
	}

	rig.r.purgeSessions(ctx) // the next sweep
	for k, wantThere := range map[store.ConversationKey]bool{past: false, left: false, inside: true, longer: true} {
		_, err := st.GetSession(ctx, k, 100*365*day, sweepNow)
		if present := err == nil; present != wantThere {
			t.Errorf("session %s present = %v (%v) after the purge; want %v", k.Thread, present, err, wantThere)
		}
	}
}

// The broker runs it: a session of the example config's Binding that has been expired for good, with a task that a crash left
// and a question nobody answered, is ended and then deleted soon after the broker is up (the agent cannot be reached from the
// test, which is a failed cancel and no obstacle), the ending is audited and counted, and one inside the horizon keeps its task.
func TestRunEndsATaskThatACrashLeftOnAnExpiredSession(t *testing.T) {
	c := startConfig(t)
	c.retentionEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	stale := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	live := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "2.2"}
	for k, idle := range map[store.ConversationKey]time.Duration{stale: 90*day + 90*day + day, live: 90*day + 89*day} { // the example's threadTTL is 90 days
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: "sre/sre-alerts", ContextID: "ctx", LastActivity: time.Now().Add(-idle)}); err != nil {
			t.Fatal(err)
		}
		if err := seed.SetSessionTask(t.Context(), k, "", "task-"+k.Thread, store.TaskTurn{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", 200)
	// The ending, and the deletion that follows it in the same tick: the session of the example's Binding that is inside its
	// horizon is the only other one there, so one session is counted as deleted.
	for _, line := range []string{`agentkourier_retention_removed_total{what="stale_tasks"} 1`, `agentkourier_retention_removed_total{what="expired_sessions"} 1`} {
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, body := r.get("/metrics"); strings.Contains(body, line+"\n") {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("/metrics never held %q; /metrics:\n%s", line, body)
			}
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.GetSession(t.Context(), stale, 100*365*day, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the stale session = %v; want it deleted by the sweep that follows the ending", err)
	}
	if s, err := st.GetSession(t.Context(), live, 100*365*day, time.Now()); err != nil || s.TaskID != "task-2.2" {
		t.Errorf("the session inside its horizon = task %q, %v; want its task left", s.TaskID, err)
	}
	entries, err := st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ended []store.AuditEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Outcome, "stale_task_ended:") {
			ended = append(ended, e)
		}
	}
	if len(ended) != 1 || ended[0].Subject != "task=task-1.1 context=ctx" || ended[0].Binding != "sre/sre-alerts" || ended[0].Kind != store.AuditEvent {
		t.Fatalf("the ending in the audit = %+v; want one, of task-1.1, by sre/sre-alerts", ended)
	}
	if ended[0].Identity == "" { // the cancel was made under the Binding's identity, as a stop's is
		t.Error("the ending was audited with no identity; want the Binding's, which the cancel was made under")
	}
	if !strings.HasPrefix(ended[0].Outcome, "stale_task_ended: cancel=failed ") { // the agent was asked, through the Binding, and did not answer
		t.Errorf("the outcome = %q; want a cancel that was made at the agent and failed", ended[0].Outcome)
	}
}
