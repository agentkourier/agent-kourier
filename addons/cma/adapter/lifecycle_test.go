package adapter

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// clock is a settable time for the executor.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// clockedExec is an executor, its store and an A2A client of it, all on one clock: the store stamps a message's arrival
// and the executor a turn's start and end by it, and sweep reads it.
type clockedExec struct {
	*Executor
	st  *store.Store
	clk *clock
}

func newClockedExec(t *testing.T, fake *fakeBackend, ttl time.Duration) (*clockedExec, *clock, *a2aclient.Client) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	st := newStore(t, c.now)
	exec := &Executor{Backend: fake, Store: st, IdleTTL: ttl, now: c.now}
	return &clockedExec{Executor: exec, st: st, clk: c}, c, serve(t, exec, st, nil)
}

// sweep does what the kit's upkeep does with an idle context, as of the clock's now: for each context idle for longer
// than IdleTTL with no turn running, it has the executor release it and, if it did, forgets it with its tasks from
// before. Zero IdleTTL keeps them. It returns the errors ReleaseContext returned.
func (e *clockedExec) sweep(t *testing.T) []error {
	t.Helper()
	if e.IdleTTL <= 0 {
		return nil
	}
	now := e.clk.now()
	idle, err := e.st.IdleContexts(t.Context(), now.Add(-e.IdleTTL))
	if err != nil {
		t.Fatal(err)
	}
	var refused []error
	for _, c := range idle {
		if err := e.ReleaseContext(t.Context(), c); err != nil {
			refused = append(refused, err)
			continue
		}
		if err := e.st.ForgetContext(t.Context(), c.Owner, c.ContextID, c.Ref, now); err != nil {
			t.Fatal(err)
		}
	}
	return refused
}

func TestIdleSessionIsArchivedAfterTTL(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	first, _ := turn(t, h, userText("", "one"))

	clk.advance(time.Hour)
	exec.sweep(t)
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived at exactly the TTL: %v", got)
	}
	if n := exec.Contexts(); n != 1 {
		t.Fatalf("contexts with a session = %d before the archive, want 1", n)
	}
	clk.advance(time.Second)
	exec.sweep(t)
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want [sesn_1]", got)
	}
	if n := exec.Contexts(); n != 0 {
		t.Fatalf("contexts with a session = %d after the archive, want 0", n)
	}

	// The context's next message starts a new session.
	turn(t, h, userText(first.ContextID, "two"))
	if n := fake.sessionCount(); n != 2 {
		t.Errorf("sessions created = %d, want 2", n)
	}
}

func TestActivityRestartsTheIdleClock(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	first, _ := turn(t, h, userText("", "one"))
	clk.advance(50 * time.Minute)
	turn(t, h, userText(first.ContextID, "two"))
	clk.advance(50 * time.Minute)
	exec.sweep(t)
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v 50 minutes after its last turn", got)
	}
}

func TestZeroTTLNeverArchives(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, 0)
	turn(t, h, userText("", "one"))
	clk.advance(10000 * time.Hour)
	exec.sweep(t)
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v with no TTL", got)
	}
}

func TestRunningTurnIsNotArchived(t *testing.T) {
	// The agent starts and then says nothing: the turn stays open until the test ends it.
	fake := newFake(script(say("thinking")))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	working := make(chan struct{})
	done := make(chan a2a.TaskState, 1)
	go func() {
		var state a2a.TaskState
		once := sync.Once{}
		for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("", "long")}) {
			if err != nil {
				t.Errorf("stream error: %v", err)
				break
			}
			if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
				once.Do(func() { close(working) })
			}
			if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				state = s.Status.State
			}
		}
		done <- state
	}()
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal("turn never started")
	}

	clk.advance(24 * time.Hour)
	exec.sweep(t)
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v during a turn", got)
	}

	fake.publish("sesn_1", idle(stopEndTurn))
	if s := <-done; s != a2a.TaskStateCompleted {
		t.Fatalf("turn ended %s, want completed", s)
	}
	// The turn's end is its last activity.
	clk.advance(59 * time.Minute)
	exec.sweep(t)
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v 59 minutes after the turn ended", got)
	}
}

func TestFailedArchiveIsRetried(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	turn(t, h, userText("", "one"))
	clk.advance(2 * time.Hour)
	fake.archiveErr = errors.New("503")
	if refused := exec.sweep(t); len(refused) != 1 || errors.Is(refused[0], runtime.ErrContextBusy) {
		t.Fatalf("the sweep's refusals = %v, want the failed archive's alone", refused)
	}
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived = %v after a failed call", got)
	}
	if n := exec.Contexts(); n != 1 {
		t.Fatalf("contexts with a session = %d after a failed archive, want the session back in the map", n)
	}
	exec.sweep(t)
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want the retry to archive sesn_1", got)
	}
}

func TestRestoreMapsContextsToSessions(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	now := clk.now()
	fake.seed("sesn_old", "ctx-1", now.Add(-10*time.Minute))
	fake.seed("sesn_new", "ctx-1", now.Add(-5*time.Minute)) // a second session for the same context: the newer one wins
	fake.seed("sesn_other", "ctx-2", now.Add(-time.Minute))
	fake.seed("sesn_foreign", "", now) // not the adapter's: no context metadata

	if err := exec.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_old"}) {
		t.Fatalf("archived = %v, want the superseded sesn_old only", got)
	}

	turn(t, h, userText("ctx-1", "again"))
	sent := fake.sentEvents()
	if len(sent) != 1 || sent[0].session != "sesn_new" {
		t.Fatalf("sent = %+v, want the message in sesn_new", sent)
	}
	if n := fake.sessionCount(); n != 0 {
		t.Errorf("sessions created = %d, want 0", n)
	}
}

func TestRestoredSessionAgesFromItsLastActivity(t *testing.T) {
	fake := newFake(nil)
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	fake.seed("sesn_stale", "ctx-1", clk.now().Add(-2*time.Hour))
	fake.seed("sesn_fresh", "ctx-2", clk.now().Add(-10*time.Minute))
	fake.seed("sesn_unknown", "ctx-3", time.Time{}) // no activity time: counted from the restore, not from year 1
	if err := exec.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	exec.sweep(t)
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_stale"}) {
		t.Fatalf("archived = %v, want [sesn_stale]", got)
	}
}

// A context that the store listed as idle, but whose session a turn here has taken since (a message that arrived during
// the sweep), is refused as busy, and its session is neither archived nor dropped from the map.
func TestATurnThatBeganSinceTheListingKeepsItsSession(t *testing.T) {
	fake := newFake(script(say("thinking")))
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	if _, err := exec.acquire(t.Context(), "c1"); err != nil {
		t.Fatal(err)
	}
	listed := kitstore.Context{Owner: auth.Caller, ContextID: "c1", Ref: "sesn_1"}
	if err := exec.ReleaseContext(t.Context(), listed); !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want ErrContextBusy", err)
	}
	if got := fake.archivedSessions(); len(got) != 0 || exec.Contexts() != 1 {
		t.Fatalf("archived %v, %d contexts mapped; want the session kept", got, exec.Contexts())
	}
	exec.release(t.Context(), "c1", "sesn_1")
	if err := exec.ReleaseContext(t.Context(), listed); !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext of the old listing once the turn has ended = %v, want ErrContextBusy: the turn ended after it", err)
	}
	clk.advance(2 * time.Hour)
	if refused := exec.sweep(t); len(refused) != 0 {
		t.Fatalf("a sweep listing the context after its turn = %v", refused)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want sesn_1", got)
	}
}

// A whole turn that runs between the sweep's listing of its idle context and the context's release keeps the session,
// and the task it finished: the session was active after the listing.
func TestATurnBetweenTheListingAndTheReleaseKeepsItsSession(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	first, _ := turn(t, h, userText("", "one"))
	clk.advance(2 * time.Hour)
	listed, err := exec.st.IdleContexts(t.Context(), clk.now().Add(-time.Hour))
	if err != nil || len(listed) != 1 {
		t.Fatalf("idle contexts = %+v, %v", listed, err)
	}
	second, _ := turn(t, h, userText(first.ContextID, "two")) // while the sweep is still on earlier contexts
	if second.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the second turn = %s", second.Status.State)
	}
	if err := exec.ReleaseContext(t.Context(), listed[0]); !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want ErrContextBusy", err)
	}
	if got := fake.archivedSessions(); len(got) != 0 || exec.Contexts() != 1 {
		t.Fatalf("archived %v, %d contexts mapped; want the session kept", got, exec.Contexts())
	}
	if _, err := h.GetTask(t.Context(), &a2a.GetTaskRequest{ID: second.ID}); err != nil {
		t.Fatalf("the task the turn finished: %v", err)
	}
}

// A stored session that is not the one in the map (a PutSession that failed after the stored one was let go of) is
// not archived: the release records the map's session in the store and leaves the context for the next sweep, which
// counts its idle time from that session's activity. A turn running in the map's session refuses the context as busy.
func TestAStoredSessionThatIsNotTheMapsIsReplacedForTheNextSweep(t *testing.T) {
	fake := newFake(nil)
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	if err := exec.st.PutSession(t.Context(), store.Session{ContextID: "c1", SessionID: "sesn_stale", LastActive: clk.now()}); err != nil {
		t.Fatal(err)
	}
	newer := &contextSession{id: "sesn_new", lastActive: clk.now().Add(time.Minute), turns: 1}
	exec.sessions = map[string]*contextSession{"c1": newer}
	clk.advance(2 * time.Hour)
	listed, err := exec.st.IdleContexts(t.Context(), clk.now().Add(-time.Hour))
	if err != nil || len(listed) != 1 || listed[0].Ref != "sesn_stale" {
		t.Fatalf("idle contexts = %+v, %v; want c1 with the stale session", listed, err)
	}
	if err := exec.ReleaseContext(t.Context(), listed[0]); !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext while a turn runs in the map's session = %v, want ErrContextBusy", err)
	}
	newer.turns = 0
	err = exec.ReleaseContext(t.Context(), listed[0])
	if err == nil || errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want an error that is not ErrContextBusy", err)
	}
	if got := fake.archivedSessions(); len(got) != 0 || exec.Contexts() != 1 {
		t.Fatalf("archived %v, %d contexts mapped; want nothing archived and the map's session kept", got, exec.Contexts())
	}
	stored, err := exec.st.Sessions(t.Context())
	if err != nil || len(stored) != 1 || stored[0].SessionID != "sesn_new" || !stored[0].LastActive.Equal(newer.lastActive) {
		t.Fatalf("stored sessions = %+v, %v; want sesn_new with its activity", stored, err)
	}
	if refused := exec.sweep(t); len(refused) != 0 {
		t.Fatalf("the next sweep = %v", refused)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_new"}) || exec.Contexts() != 0 {
		t.Fatalf("archived %v, %d contexts mapped; want sesn_new archived", got, exec.Contexts())
	}
}

// A context whose session is still being created is refused as busy: the creation's PutSession would follow the
// forget.
func TestAContextWhoseSessionIsBeingCreatedIsBusy(t *testing.T) {
	fake := newFake(nil)
	g := newGate(t)
	holdCreateOf(fake, "c1", g)
	exec, _, _ := newClockedExec(t, fake, time.Hour)
	created := acquireAsync(t.Context(), exec.Executor, "c1")
	g.reached(t)
	if err := exec.ReleaseContext(t.Context(), kitstore.Context{Owner: auth.Caller, ContextID: "c1"}); !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want ErrContextBusy", err)
	}
	g.release()
	if r := acquired(t, created, "the acquire"); r.err != nil {
		t.Fatal(r.err)
	}
}

// What a context with no stored session gets: the one in the map, which a failed PutSession left out of the store, is
// archived; with none in the map either, nothing is, and the release lets the kit forget the context.
func TestAContextWithNoStoredSession(t *testing.T) {
	fake := newFake(nil)
	exec, _, _ := newClockedExec(t, fake, time.Hour)
	exec.sessions = map[string]*contextSession{"mapped": {id: "sesn_mapped"}}
	for _, c := range []string{"mapped", "none"} {
		if err := exec.ReleaseContext(t.Context(), kitstore.Context{Owner: auth.Caller, ContextID: c}); err != nil {
			t.Fatalf("ReleaseContext %s = %v", c, err)
		}
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_mapped"}) || exec.Contexts() != 0 {
		t.Fatalf("archived %v, %d contexts mapped; want the mapped session alone archived", got, exec.Contexts())
	}
}

// The store's row of a context whose first message arrived but whose session was never created (CMA refused it) has no
// session: the sweep forgets it without an archive.
func TestASweepForgetsAContextThatNeverHadASession(t *testing.T) {
	fake := newFake(nil)
	fake.createErr = errors.New("CMA unavailable")
	exec, clk, h := newClockedExec(t, fake, time.Hour)
	for _, err := range h.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: userText("c-none", "hi")}) {
		_ = err
	}
	if all, err := exec.st.Contexts(t.Context()); err != nil || len(all) != 1 || all[0].Ref != "" {
		t.Fatalf("contexts = %+v, %v; want c-none's, with no session", all, err)
	}
	clk.advance(2 * time.Hour)
	if refused := exec.sweep(t); len(refused) != 0 {
		t.Fatalf("refused %v", refused)
	}
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v for a context with no session", got)
	}
	if all, err := exec.st.Contexts(t.Context()); err != nil || len(all) != 0 {
		t.Fatalf("contexts = %+v, %v; want none", all, err)
	}
}

// flakyTouch is the executor's store with its next failures writes of a session's activity refused.
type flakyTouch struct {
	Store
	mu       sync.Mutex
	failures int
}

func (f *flakyTouch) failNext(n int) { f.mu.Lock(); defer f.mu.Unlock(); f.failures = n }

func (f *flakyTouch) TouchSession(ctx context.Context, contextID, sessionID string, at time.Time) error {
	f.mu.Lock()
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("database is locked")
	}
	return f.Store.TouchSession(ctx, contextID, sessionID, at)
}

// A turn's activity that the store failed to record leaves the store behind the map: the sweep that finds it so is
// refused as busy and writes the activity again, so that a later sweep past the TTL archives the session.
func TestAnActivityTheStoreMissedIsWrittenAgainForTheNextSweep(t *testing.T) {
	var clk *clock
	fake := newFake(func(session string, out Outbound) []Event {
		clk.advance(time.Minute) // the turn runs a minute after its message arrived
		return script(say("ok"), idle(stopEndTurn))(session, out)
	})
	exec, c, h := newClockedExec(t, fake, time.Hour)
	clk = c
	flaky := &flakyTouch{Store: exec.st}
	exec.Store = flaky
	first, _ := turn(t, h, userText("", "one"))
	flaky.failNext(2) // the second turn's start and end
	turn(t, h, userText(first.ContextID, "two"))

	clk.advance(2 * time.Hour)
	if refused := exec.sweep(t); len(refused) != 1 || !errors.Is(refused[0], runtime.ErrContextBusy) {
		t.Fatalf("the first sweep's refusals = %v, want busy: the store's activity is behind the map's", refused)
	}
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v by the refused sweep", got)
	}
	clk.advance(time.Minute)
	if refused := exec.sweep(t); len(refused) != 0 {
		t.Fatalf("the next sweep's refusals = %v, want none: the refused sweep wrote the activity again", refused)
	}
	if got := fake.archivedSessions(); !slices.Equal(got, []string{"sesn_1"}) {
		t.Fatalf("archived = %v, want [sesn_1]", got)
	}
}

// A context whose stored row names no session, while the map has one active since the listing (its PutSession was
// lost), is refused as busy and has the map's session recorded, with its activity, for the next sweep.
func TestAMappedSessionTheStoreMissedIsRecordedWhenBusy(t *testing.T) {
	fake := newFake(nil)
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	listed := clk.now()
	exec.sessions = map[string]*contextSession{"c1": {id: "sesn_mapped", lastActive: listed.Add(time.Minute)}}
	err := exec.ReleaseContext(t.Context(), kitstore.Context{Owner: auth.Caller, ContextID: "c1", LastActive: listed})
	if !errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want busy", err)
	}
	stored, err := exec.st.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Session{{ContextID: "c1", SessionID: "sesn_mapped", LastActive: listed.Add(time.Minute)}}
	if len(stored) != 1 || stored[0].ContextID != want[0].ContextID || stored[0].SessionID != want[0].SessionID ||
		!stored[0].LastActive.Equal(want[0].LastActive) {
		t.Fatalf("stored sessions = %+v, want %+v", stored, want)
	}
	if got := fake.archivedSessions(); len(got) != 0 {
		t.Fatalf("archived %v", got)
	}
}

// A release refused as busy whose write of the activity fails says so as a failure, which the upkeep logs at WARN.
func TestAFailedWriteOfAMissedActivityIsNotBusy(t *testing.T) {
	fake := newFake(nil)
	exec, clk, _ := newClockedExec(t, fake, time.Hour)
	flaky := &flakyTouch{Store: exec.st}
	exec.Store = flaky
	listed := clk.now()
	exec.sessions = map[string]*contextSession{"c1": {id: "sesn_1", lastActive: listed.Add(time.Minute)}}
	flaky.failNext(1)
	err := exec.ReleaseContext(t.Context(), kitstore.Context{Owner: auth.Caller, ContextID: "c1", Ref: "sesn_1", LastActive: listed})
	if err == nil || errors.Is(err, runtime.ErrContextBusy) {
		t.Fatalf("ReleaseContext = %v, want the store's failure, not busy", err)
	}
	if got := fake.archivedSessions(); len(got) != 0 || exec.Contexts() != 1 {
		t.Fatalf("archived %v, %d contexts mapped; want the session kept", got, exec.Contexts())
	}
}
