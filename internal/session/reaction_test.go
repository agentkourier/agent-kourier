package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// dequeued is the sink's NoticeDequeued notices, as "message ref" pairs, in order.
func (s *fakeSink) dequeued() (out []string) {
	for _, n := range s.noticesOf(NoticeDequeued) {
		out = append(out, n.ID+" "+n.MessageRef)
	}
	return out
}

// A reply that waited keeps its reaction only while it waits: the notice that takes it off comes when the agent
// has the turn, not at the end of its output, and it is raised for that turn alone. The turn that never waited
// has none to take off.
func TestQueuedReplysReactionIsTakenOffWhenItIsSent(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	first, second := make(chan struct{}), make(chan struct{})
	running := make(chan int, 2)
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		running <- n
		if n == 0 {
			<-first
		} else {
			<-second
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if res, err := f.submit(inbound("1")); err != nil || res.Queued {
		t.Fatalf("first turn = %+v, %v; want it not queued", res, err)
	}
	<-running // the task runs
	in := inbound("2")
	if res, err := f.submit(in); err != nil || !res.Queued {
		t.Fatalf("second turn = %+v, %v; want it queued", res, err)
	}
	if got := sink.dequeued(); len(got) != 0 {
		t.Fatalf("dequeued %q while the reply waits; want none", got)
	}
	close(first)
	<-running // the second turn is sent, and its task runs on
	want := []string{in.MessageID + "/queued 2"}
	if got := sink.dequeued(); !slices.Equal(got, want) {
		t.Fatalf("dequeued %q while the second turn runs; want %q: the reaction goes when the turn is sent", got, want)
	}
	close(second)
	waitFinal(t, sink)
	waitFinal(t, sink)
	f.retired(t)
	if got := sink.dequeued(); !slices.Equal(got, want) {
		t.Errorf("dequeued %q after both turns ended; want %q once, and none for the turn that never waited", got, want)
	}
}

// The reaction goes on every ending of a turn. Most are after the agent has the turn, and the reaction came off then;
// the rest never get the turn named a task (it is refused, cannot be reached for good, or is dropped for a Binding
// that left the config), and would keep their hourglass for ever.
func TestQueuedReplysReactionIsTakenOffWhenTheTurnEnds(t *testing.T) {
	ended := func(state a2a.TaskState) func(agent.Handler, int) error {
		return func(h agent.Handler, n int) error { return turn(h, n, state) }
	}
	for _, tc := range []struct {
		name string
		// second is what the second turn's sends do; it is not called if the turn is dropped for its Binding.
		second      func(h agent.Handler, n int) error
		bindingGone bool
		want        TurnOutcome
	}{
		{"the agent refuses the identity", func(agent.Handler, int) error { return agent.ErrForbidden }, false, TurnForbidden},
		{"the front door refuses the credentials", func(agent.Handler, int) error {
			return fmt.Errorf("%w: %w", agent.ErrNotSent, agent.ErrCredentialsRejected)
		}, false, TurnCredentialsRejected},
		{"the agent cannot be reached for good", func(agent.Handler, int) error { return fmt.Errorf("%w: down", agent.ErrNotSent) }, false, TurnUnreachable},
		{"the Binding left the config", nil, true, TurnBindingGone},
		{"the task completes", ended(a2a.TaskStateCompleted), false, TurnCompleted},
		{"the task fails", ended(a2a.TaskStateFailed), false, TurnFailed},
		{"the task is rejected", ended(a2a.TaskStateRejected), false, TurnRejected},
		{"the task is cancelled", ended(a2a.TaskStateCanceled), false, TurnCanceled},
		{"the agent refuses to go on with the task", func(h agent.Handler, n int) error {
			accepted(h, n)
			return errors.New("boom")
		}, false, TurnRefused},
		{"the agent no longer has the session", func(h agent.Handler, n int) error {
			accepted(h, n)
			return &agent.StreamLostError{TaskID: "t2", ContextID: "c1", Err: errors.New("connection reset")}
		}, false, TurnSessionGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, a2a.ErrTaskNotFound }
			outcomes := make(chan TurnOutcome, 4)
			f.m.cfg.OnTurnEnd = func(_ string, o TurnOutcome) { outcomes <- o }
			gate := make(chan struct{})
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				if n == 0 {
					<-gate
					return turn(h, n, a2a.TaskStateCompleted)
				}
				f.now.Add(int64(time.Hour)) // time passes, so that an unreachable agent is given up on
				return tc.second(h, n)
			}
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			f.nextSent() // the first turn is with the agent
			in := inbound("2")
			if res, err := f.submit(in); err != nil || !res.Queued {
				t.Fatalf("second turn = %+v, %v; want it queued", res, err)
			}
			if tc.bindingGone {
				f.amu.Lock()
				f.bindingErr = ErrUnknownBinding
				f.amu.Unlock()
			}
			close(gate)
			for i, want := range []TurnOutcome{TurnCompleted, tc.want} { // the first turn ends, and then the second
				select {
				case got := <-outcomes:
					if got != want {
						t.Fatalf("turn %d ended %s; want %s", i+1, got, want)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("turn %d did not end %s", i+1, want)
				}
			}
			want := []string{in.MessageID + "/queued 2"}
			if got := sink.dequeued(); !slices.Equal(got, want) {
				t.Errorf("dequeued %q after the turn ended %s; want %q", got, tc.want, want)
			}
		})
	}
}

// A reply that got -32004 from a task nobody recorded is marked by the runner, not Submit, and the mark comes off
// when the turn is at last sent.
func TestBusyRepliesReactionIsTakenOffWhenItIsFinallySent(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n < 3 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	in := inbound("1")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	f.retired(t)
	var kinds []NoticeKind
	sink.mu.Lock()
	for _, n := range sink.notices {
		if n.Kind == NoticeQueued || n.Kind == NoticeDequeued {
			kinds = append(kinds, n.Kind)
		}
	}
	sink.mu.Unlock()
	if want := []NoticeKind{NoticeQueued, NoticeDequeued}; !slices.Equal(kinds, want) {
		t.Errorf("notices = %v; want the reaction asked for once and taken off once, in that order", kinds)
	}
	if got, want := sink.dequeued(), []string{in.MessageID + "/queued 1"}; !slices.Equal(got, want) {
		t.Errorf("dequeued %q; want %q", got, want)
	}
}

// A turn that an earlier process queued has no mark in this one, and may carry the reaction: it is taken off when
// the turn is sent, which the sink finds harmless if there was none.
func TestReplyQueuedByAnEarlierProcessHasItsReactionTakenOff(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	in := inbound("1")
	if _, err := f.st.EnqueueReply(t.Context(), store.Reply{ID: in.MessageID, Key: key, UserID: in.UserID, MessageRef: in.MessageRef, Text: in.Text, EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	f.m.wake(key) // the restarted process finds it in the queue
	waitFinal(t, sink)
	f.retired(t)
	if got, want := sink.dequeued(), []string{in.MessageID + "/queued 1"}; !slices.Equal(got, want) {
		t.Errorf("dequeued %q; want %q", got, want)
	}
}

// The notice that asks for the reaction comes before the wake that lets the runner send the turn and take it off,
// so the two reach the sink in that order.
func TestQueuedNoticeComesBeforeTheWake(t *testing.T) {
	f := newFixture(t)
	sink := &fakeSink{finals: make(chan struct{}, 4)}
	f.m.cfg.Sink = sink
	var noticesAtWake int
	f.m.wake = func(Key) { noticesAtWake = len(sink.noticesOf(NoticeQueued)) } // no runner: nothing is sent
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	if noticesAtWake != 1 {
		t.Errorf("the queued reply's wake saw %d queued notices; want 1, raised before it", noticesAtWake)
	}
}

// A duplicate of a turn's event that Submit sees after the turn was sent is no waiting turn: it raises no
// notice and is not marked, for nothing would ever take its mark off.
func TestDuplicateOfASentTurnIsNotMarkedQueued(t *testing.T) {
	f := newFixture(t)
	sink := &fakeSink{finals: make(chan struct{}, 4)}
	f.m.cfg.Sink = sink
	for _, ts := range []string{"1", "2", "3"} { // no runner, so 2 and 3 are queued behind 1
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	third := inbound("3")
	f.m.mu.Lock()
	delete(f.m.fresh, third.MessageID) // as the runner does when the agent has taken it
	delete(f.m.marked, third.MessageID)
	f.m.mu.Unlock()
	if _, err := f.submit(third); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	_, marked := f.m.marked[third.MessageID]
	f.m.mu.Unlock()
	if got := len(sink.noticesOf(NoticeQueued)); marked || got != 2 {
		t.Errorf("marked %t, %d queued notices; want the duplicate of a sent turn left unmarked and silent, after the two real ones", marked, got)
	}
}

// -32004 from a session that is not ready (the dialect says so) is not "behind another turn": the reply gets no
// reaction, not even the first mention of a thread whose session is still starting. It is sent again, as ever.
func TestNotReadySessionIsNotQueued(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	var sends atomic.Int32
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		sends.Add(1)
		if n < 4 {
			return fmt.Errorf("send message: %w: %w", agent.ErrNotReady, agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	f.retired(t)
	if got := sink.noticesOf(NoticeQueued); len(got) != 0 {
		t.Errorf("queued notices = %+v; want none: nothing is ahead of this turn", got)
	}
	if got := sink.dequeued(); len(got) != 0 {
		t.Errorf("dequeued %q; want none: no reaction was asked for", got)
	}
	if n := sends.Load(); n != 5 {
		t.Errorf("%d sends; want the turn sent again until the agent took it, 5", n)
	}
}

// A turn that keeps getting -32004 tells the thread once, after waitNoticeAfter and not before, and goes on being
// sent: busy and not ready each say their own. The notice is named after the turn and its kind.
func TestStuckTurnTellsTheThreadOnceAndKeepsTrying(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		kind      NoticeKind
		wantQueue int // queued notices: a busy session queues the reply, one not ready does not
	}{
		{"busy", fmt.Errorf("send message: %w", agent.ErrBusy), NoticeStillBusy, 1},
		{"not ready", fmt.Errorf("send message: %w: %w", agent.ErrNotReady, agent.ErrBusy), NoticeNotReady, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			var early []Notice // the notices of this kind before the wait is long enough
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				switch n {
				case 3:
					f.now.Store(t0.Add(waitNoticeAfter - time.Second).UnixNano())
				case 4:
					early = sink.noticesOf(tc.kind)
					f.now.Store(t0.Add(waitNoticeAfter).UnixNano())
				case 8:
					return turn(h, n, a2a.TaskStateCompleted)
				}
				return tc.err
			}
			in := inbound("1")
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			f.retired(t)
			if len(early) != 0 {
				t.Errorf("notices %+v before the wait was long; want none", early)
			}
			got := sink.noticesOf(tc.kind)
			if len(got) != 1 || got[0].ID != in.MessageID+"/"+string(tc.kind) || got[0].MessageRef != "1" {
				t.Errorf("%s notices = %+v; want one, after the wait, named after the turn", tc.kind, got)
			}
			f.agent.mu.Lock()
			sent := len(f.agent.msgs)
			f.agent.mu.Unlock()
			if sent != 9 {
				t.Errorf("%d sends; want the turn sent again after the notice, until the agent took it: 9", sent)
			}
			if got := len(sink.noticesOf(NoticeQueued)); got != tc.wantQueue {
				t.Errorf("%d queued notices, want %d", got, tc.wantQueue)
			}
		})
	}
}

// The wait is counted for the turn that waits, not for the thread: the next turn that gets -32004 starts its own.
func TestTheWaitIsCountedPerTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		switch n {
		case 0, 2: // the first send of each turn is busy
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		case 1:
			f.now.Store(t0.Add(time.Hour).UnixNano()) // long after the first turn's first -32004
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	f.retired(t)
	if got := sink.noticesOf(NoticeStillBusy); len(got) != 0 {
		t.Errorf("still-busy notices = %+v; want none: no turn waited long", got)
	}
}
