package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

// adapterRun is one start of the adapter over a store file: what main does, without the network.
type adapterRun struct {
	exec  *Executor
	h     a2asrv.RequestHandler
	store *store.Store
}

// startAdapter opens the store at path, restores and settles as main does, and serves through a handler. The fake is
// CMA, which outlives the adapter.
func startAdapter(t *testing.T, path string, fake *fakeBackend, clk *clock) *adapterRun {
	t.Helper()
	st := openStore(t, path, clk.now)
	exec := &Executor{Backend: fake, Store: st, IdleTTL: time.Hour, TaskRetention: 24 * time.Hour, now: clk.now}
	if err := exec.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := exec.Settle(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	return &adapterRun{exec: exec, h: NewRequestHandler(exec, asKourier{}), store: st}
}

// stop closes the run's store, releasing the file for the next run.
func (r *adapterRun) stop(t *testing.T) {
	t.Helper()
	if err := r.store.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)} }

func TestRestartContinuesTheContextInItsSession(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	path, clk := filepath.Join(t.TempDir(), "cma.db"), newClock()
	first := startAdapter(t, path, fake, clk)
	task, _ := turn(t, first.h, userText("", "one"))
	first.stop(t)

	second := startAdapter(t, path, fake, clk)
	turn(t, second.h, userText(task.ContextID, "two"))
	if n := fake.sessionCount(); n != 1 {
		t.Fatalf("sessions created = %d, want 1: a restart started a second session for the context", n)
	}
	sent := fake.sentEvents()
	if last := sent[len(sent)-1]; last.session != "sesn_1" || last.out.Message == nil || *last.out.Message != "two" {
		t.Fatalf("last sent = %+v, want two in sesn_1", last)
	}
}

func TestResendAfterRestartJoinsTheTask(t *testing.T) {
	fake := newFake(script(say("once"), idle(stopEndTurn)))
	path, clk := filepath.Join(t.TempDir(), "cma.db"), newClock()
	first := startAdapter(t, path, fake, clk)
	msg := userText("", "hi")
	task, _ := turn(t, first.h, msg)
	first.stop(t)

	second := startAdapter(t, path, fake, clk)
	again := *msg
	again.ContextID = task.ContextID
	joined, _ := turn(t, second.h, &again)
	if joined.ID != task.ID || joined.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("resend after a restart: task %s %s, want %s completed", joined.ID, joined.Status.State, task.ID)
	}
	if n := len(fake.sentEvents()); n != 1 {
		t.Fatalf("messages sent to the session = %d, want 1", n)
	}
}

// A pause survives a restart, and its answer reaches the paused call.
func TestApprovalAfterRestartReachesThePausedCall(t *testing.T) {
	fake := newFake(approvalScript)
	path, clk := filepath.Join(t.TempDir(), "cma.db"), newClock()
	first := startAdapter(t, path, fake, clk)
	paused, _ := turn(t, first.h, userText("", "clean up"))
	if paused.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want input-required", paused.Status.State)
	}
	first.stop(t)

	second := startAdapter(t, path, fake, clk)
	still, err := second.h.GetTask(context.Background(), &a2a.GetTaskRequest{ID: paused.ID})
	if err != nil || still.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("after the restart: %v, state %v; want the task still paused", err, still.Status.State)
	}
	done, _ := turn(t, second.h, approvalReply(paused, true, ""))
	if done.ID != paused.ID || done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("after allow: task %s state %s, want %s completed", done.ID, done.Status.State, paused.ID)
	}
	sent := fake.sentEvents()
	last := sent[len(sent)-1]
	if c := last.out.Confirmation; c == nil || c.ToolUseID != "sevt_ask" || !c.Allow || last.session != "sesn_1" {
		t.Fatalf("last sent = %+v, want an allow for sevt_ask in sesn_1", last)
	}
	if n := fake.sessionCount(); n != 1 {
		t.Errorf("sessions created = %d, want 1", n)
	}
}

func TestRestartFailsTheTurnsItCutOff(t *testing.T) {
	fake := newFake(nil)
	path, clk := filepath.Join(t.TempDir(), "cma.db"), newClock()
	// What a crash leaves: tasks in flight (one whose stored record does not decode), a paused one, a settled one, and
	// the contexts' sessions.
	st := openStore(t, path, clk.now)
	ctx := kourier(context.Background())
	for _, tk := range []*a2a.Task{
		{ID: "submitted", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		{ID: "working", ContextID: "c2", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		{ID: "paused", ContextID: "c3", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}},
		{ID: "done", ContextID: "c3", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
		{ID: "unreadable", ContextID: "c4", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
	} {
		if _, err := st.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	corrupt(t, st, "unreadable")
	for i, c := range []string{"c1", "c2", "c3", "c4"} {
		id := "sesn_" + c
		fake.seed(id, c, clk.now())
		if err := st.PutSession(ctx, store.Session{ContextID: c, SessionID: id, LastActive: clk.now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	run := startAdapter(t, path, fake, clk)
	for id, want := range map[a2a.TaskID]a2a.TaskState{
		"submitted": a2a.TaskStateFailed, "working": a2a.TaskStateFailed,
		"paused": a2a.TaskStateInputRequired, "done": a2a.TaskStateCompleted,
	} {
		task, err := run.h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatalf("GetTask %s: %v", id, err)
		}
		if task.Status.State != want {
			t.Errorf("%s: state %s, want %s", id, task.Status.State, want)
		}
		if want == a2a.TaskStateFailed {
			if got := statusText(task); got != InterruptedMessage || task.Status.Message.Role != a2a.MessageRoleAgent {
				t.Errorf("%s: status message %q, want the agent saying %q", id, got, InterruptedMessage)
			}
		}
	}
	var interrupted []string
	for _, s := range fake.sentEvents() {
		if s.out.Interrupt {
			interrupted = append(interrupted, s.session)
		}
	}
	slices.Sort(interrupted)
	if !slices.Equal(interrupted, []string{"sesn_c1", "sesn_c2", "sesn_c4"}) {
		t.Fatalf("interrupted %v, want the sessions of the cut-off turns alone", interrupted)
	}
}

// An interrupt that fails does not stop the adapter: the task is failed all the same.
func TestSettleSurvivesAFailedInterrupt(t *testing.T) {
	fake := newFake(nil)
	fake.sendErr = errors.New("503")
	clk := newClock()
	st := newStore(t, clk.now)
	ctx := kourier(context.Background())
	if _, err := st.Create(ctx, &a2a.Task{ID: "working", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatal(err)
	}
	fake.seed("sesn_c1", "c1", clk.now())
	exec := &Executor{Backend: fake, Store: st, now: clk.now}
	if err := exec.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := exec.Settle(ctx); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	got, err := st.Get(ctx, "working")
	if err != nil || got.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("task = %+v, %v; want failed", got, err)
	}
}

func TestRestorePrefersTheStoredSession(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	clk := newClock()
	st := newStore(t, clk.now)
	ctx := context.Background()
	// The store names the older session: a crash after creating the newer one, before storing it, left two.
	fake.seed("sesn_stored", "c1", clk.now().Add(-time.Hour))
	fake.seed("sesn_stray", "c1", clk.now().Add(-time.Minute))
	if err := st.PutSession(ctx, store.Session{ContextID: "c1", SessionID: "sesn_stored", LastActive: clk.now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	exec := &Executor{Backend: fake, Store: st, now: clk.now}
	if err := exec.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_stray"}) {
		t.Fatalf("archived = %v, want the stray session", got)
	}
	turn(t, NewRequestHandler(exec, asKourier{}), userText("c1", "again"))
	if sent := fake.sentEvents(); len(sent) != 1 || sent[0].session != "sesn_stored" {
		t.Fatalf("sent = %+v, want the message in the stored session", sent)
	}
}

func TestRestoreForgetsASessionCMANoLongerLists(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	clk := newClock()
	st := newStore(t, clk.now)
	ctx := kourier(context.Background())
	if err := st.PutSession(ctx, store.Session{ContextID: "c1", SessionID: "sesn_archived", LastActive: clk.now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, &a2a.Task{ID: "old", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	exec := &Executor{Backend: fake, Store: st, now: clk.now}
	if err := exec.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Sessions(ctx); err != nil || len(got) != 0 {
		t.Fatalf("stored sessions = %+v, %v; want none", got, err)
	}
	if _, err := st.Get(ctx, "old"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("the forgotten context's task: %v, want deleted", err)
	}
	turn(t, NewRequestHandler(exec, asKourier{}), userText("c1", "again"))
	if n := fake.sessionCount(); n != 1 {
		t.Fatalf("sessions created = %d, want a new one for the context", n)
	}
}

func TestRestoreStoresAdoptedSessions(t *testing.T) {
	fake := newFake(nil)
	clk := newClock()
	st := newStore(t, clk.now)
	fake.seed("sesn_1", "c1", clk.now().Add(-time.Minute))
	exec := &Executor{Backend: fake, Store: st, now: clk.now}
	if err := exec.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions(context.Background())
	if err != nil || len(got) != 1 || got[0].SessionID != "sesn_1" || !got[0].LastActive.Equal(clk.now().Add(-time.Minute)) {
		t.Fatalf("stored sessions = %+v, %v; want sesn_1 for c1 with its activity", got, err)
	}
}

func TestStoreFailureStopsRestore(t *testing.T) {
	fake := newFake(nil)
	st := newStore(t, nil)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	exec := &Executor{Backend: fake, Store: st}
	if err := exec.Restore(context.Background()); err == nil {
		t.Fatal("Restore succeeded on a closed store")
	}
	if err := exec.Settle(context.Background()); err == nil {
		t.Fatal("Settle succeeded on a closed store")
	}
}

func TestRetentionDeletesOldSettledTasks(t *testing.T) {
	fake := newFake(approvalScript)
	exec, clk, h := newClockedExec(t, fake, 0)
	exec.TaskRetention = 24 * time.Hour
	ctx := context.Background()
	paused, _ := turn(t, h, userText("", "clean up"))
	asked, _ := turn(t, h, userText("", "clean up too"))
	finished, _ := turn(t, h, approvalReply(asked, true, ""))
	if finished.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", finished.Status.State)
	}

	clk.advance(24 * time.Hour)
	exec.prune(ctx)
	if _, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: finished.ID}); err != nil {
		t.Fatalf("deleted at exactly the retention: %v", err)
	}
	clk.advance(time.Second)
	exec.prune(ctx)
	if _, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: finished.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("settled task after the retention: %v, want deleted", err)
	}
	for _, m := range finished.History {
		if task, err := exec.Store.MessageTask(kourier(ctx), m.ID); err != nil || task != nil {
			t.Fatalf("message %s of the deleted task: %+v, %v; want deleted", m.ID, task, err)
		}
	}
	// A pause is not settled: retention keeps it.
	if task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: paused.ID}); err != nil || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("paused task: %v; want kept", err)
	}
}

func TestZeroRetentionKeepsTasks(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, 0)
	task, _ := turn(t, h, userText("", "one"))
	clk.advance(100000 * time.Hour)
	exec.prune(context.Background())
	if _, err := h.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); err != nil {
		t.Fatalf("task with no retention: %v", err)
	}
}

func TestArchivingASessionDeletesItsContextsTasks(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	ctx := context.Background()
	old, _ := turn(t, h, userText("", "one"))
	other, _ := turn(t, h, userText("", "two"))
	clk.advance(30 * time.Minute)
	turn(t, h, userText(other.ContextID, "keep this context alive"))
	clk.advance(31 * time.Minute)
	exec.reap(ctx)
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want sesn_1", got)
	}
	if _, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: old.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("task of the archived session: %v, want deleted", err)
	}
	if _, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: other.ID}); err != nil {
		t.Fatalf("task of a live context: %v, want kept", err)
	}
	sessions, err := exec.Store.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ContextID != other.ContextID {
		t.Fatalf("stored sessions = %+v, %v; want the live context's alone", sessions, err)
	}
}

// Every accepted caller is Agent Kourier, whom the caller check's interceptor runs each call as: the store keeps the
// task, and the message that started it, for that caller, and refuses a read made for no caller.
func TestTaskOwnerIsTheCaller(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	st := newStore(t, nil)
	h := NewRequestHandler(&Executor{Backend: fake, Store: st}, asKourier{})
	msg := userText("", "hi")
	var id a2a.TaskID
	for ev, err := range h.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatal(err)
		}
		id = ev.TaskInfo().TaskID
	}
	got, err := st.Get(kourier(context.Background()), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != auth.Caller {
		t.Fatalf("owner = %q, want %s", got.User, auth.Caller)
	}
	if task, err := st.MessageTask(kourier(context.Background()), msg.ID); err != nil || task == nil || task.ID != id {
		t.Fatalf("the message's task = %+v, %v; want %s", task, err, id)
	}
	if _, err := st.Get(context.Background(), id); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Fatalf("a read for no caller: %v, want ErrUnauthenticated", err)
	}
	if _, err := h.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id}); err != nil {
		t.Fatalf("GetTask through the interceptor: %v", err)
	}
}
