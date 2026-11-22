package adapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// waitLimit bounds how long a test waits for something that should happen at once; quiet is how long a test watches
// for something that should not happen.
const (
	waitLimit = 3 * time.Second
	quiet     = 100 * time.Millisecond
)

// gate holds a call open until it is released: entered is closed once a call reaches it.
type gate struct {
	entered     chan struct{}
	released    chan struct{}
	enter, free sync.Once
}

// newGate returns a gate the test releases when it ends, so that no held call outlives it.
func newGate(t *testing.T) *gate {
	t.Helper()
	g := &gate{entered: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(g.release)
	return g
}

// hold blocks until the gate is released.
func (g *gate) hold() {
	g.enter.Do(func() { close(g.entered) })
	<-g.released
}

func (g *gate) release() { g.free.Do(func() { close(g.released) }) }

// reached waits for a call to reach the gate.
func (g *gate) reached(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(waitLimit):
		t.Fatal("no call reached the gate")
	}
}

// holdSendOf makes the fake hold the Send of the message with the given text at g.
func holdSendOf(fake *fakeBackend, text string, g *gate) {
	fake.beforeSend = func(_ string, events []Outbound) {
		for _, ev := range events {
			if ev.Message != nil && *ev.Message == text {
				g.hold()
			}
		}
	}
}

// sent is how a message sent in the background ended: the task it settled in, or the error that stopped it.
type sent struct {
	task *a2a.Task
	err  error
}

// sendAsync sends msg in a goroutine.
func sendAsync(ctx context.Context, h a2asrv.RequestHandler, msg *a2a.Message) <-chan sent {
	done := make(chan sent, 1)
	go func() {
		var id a2a.TaskID
		for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				done <- sent{err: err}
				return
			}
			id = ev.TaskInfo().TaskID
		}
		task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		done <- sent{task: task, err: err}
	}()
	return done
}

// settled waits for a background send and returns its task, which must have completed.
func settled(t *testing.T, done <-chan sent, what string) *a2a.Task {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		if r.task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("%s: state %s, want completed", what, r.task.Status.State)
		}
		return r.task
	case <-time.After(waitLimit):
		t.Fatalf("%s did not settle within %s", what, waitLimit)
	}
	return nil
}

// A message whose Send to CMA is slow holds back no other message: a new turn in another thread starts and ends.
func TestSlowSendDoesNotHoldBackAnotherMessage(t *testing.T) {
	fake := newFake(script(say("ok"), idle(stopEndTurn)))
	g := newGate(t)
	holdSendOf(fake, "A", g)
	h := newHandler(t, fake, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*waitLimit)
	defer cancel()

	a := sendAsync(ctx, h, userText("", "A"))
	g.reached(t)
	settled(t, sendAsync(ctx, h, userText("", "B")), "message B, while message A's Send was held")
	g.release()
	settled(t, a, "message A")
}

// A resend that arrives while the first send of its message ID is still in CMA waits for it and joins its task.
func TestResendDuringTheFirstSendJoinsItsTask(t *testing.T) {
	fake := newFake(script(say("once"), idle(stopEndTurn)))
	g := newGate(t)
	holdSendOf(fake, "hi", g)
	h := newHandler(t, fake, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 4*waitLimit)
	defer cancel()

	msg := userText("ctx-resend", "hi")
	first := sendAsync(ctx, h, msg)
	g.reached(t)
	again := *msg
	second := sendAsync(ctx, h, &again)
	select {
	case r := <-second:
		t.Fatalf("the resend ended (%v) while the first send was still held", r.err)
	case <-time.After(quiet):
	}
	g.release()
	one, two := settled(t, first, "the first send"), settled(t, second, "the resend")
	if two.ID != one.ID {
		t.Fatalf("resend made task %s, want %s", two.ID, one.ID)
	}
	if n := len(fake.sentEvents()); n != 1 {
		t.Fatalf("messages sent to the session = %d, want 1", n)
	}
}

// acquireAsync acquires the context's session in a goroutine.
func acquireAsync(ctx context.Context, e *Executor, contextID string) <-chan sentSession {
	done := make(chan sentSession, 1)
	go func() {
		id, err := e.acquire(ctx, contextID)
		done <- sentSession{id: id, err: err}
	}()
	return done
}

type sentSession struct {
	id  string
	err error
}

func acquired(t *testing.T, done <-chan sentSession, what string) sentSession {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(waitLimit):
		t.Fatalf("%s did not return within %s", what, waitLimit)
	}
	return sentSession{}
}

// holdCreateOf makes the fake hold the creation of the context's session at g.
func holdCreateOf(fake *fakeBackend, contextID string, g *gate) {
	fake.beforeCreate = func(c string) {
		if c == contextID {
			g.hold()
		}
	}
}

// A context whose session CMA is slow to create holds back no other context.
func TestSlowSessionCreateDoesNotHoldBackAnotherContext(t *testing.T) {
	fake := newFake(nil)
	g := newGate(t)
	holdCreateOf(fake, "slow", g)
	e := &Executor{Backend: fake, Store: newStore(t, nil)}
	ctx := context.Background()

	slow := acquireAsync(ctx, e, "slow")
	g.reached(t)
	if r := acquired(t, acquireAsync(ctx, e, "quick"), "the other context's acquire"); r.err != nil {
		t.Fatalf("acquire quick: %v", r.err)
	}
	g.release()
	if r := acquired(t, slow, "the slow acquire"); r.err != nil {
		t.Fatalf("acquire slow: %v", r.err)
	}
}

// Turns that reach a new context together share the one session the first creates, and the store has it before any
// of them runs.
func TestFirstTurnsOfAContextShareOneSession(t *testing.T) {
	fake := newFake(nil)
	g := newGate(t)
	holdCreateOf(fake, "ctx", g)
	st := newStore(t, nil)
	e := &Executor{Backend: fake, Store: st}
	ctx := context.Background()

	first := acquireAsync(ctx, e, "ctx")
	g.reached(t)
	second := acquireAsync(ctx, e, "ctx")
	select {
	case r := <-second:
		t.Fatalf("the second acquire returned (%q, %v) before the session was created", r.id, r.err)
	case <-time.After(quiet):
	}
	g.release()
	one, two := acquired(t, first, "the first acquire"), acquired(t, second, "the second acquire")
	if one.err != nil || two.err != nil {
		t.Fatalf("acquire: %v, %v", one.err, two.err)
	}
	if one.id != two.id || fake.sessionCount() != 1 {
		t.Fatalf("sessions %q and %q, %d created; want one shared", one.id, two.id, fake.sessionCount())
	}
	stored, err := st.Sessions(ctx)
	if err != nil || len(stored) != 1 || stored[0].SessionID != one.id {
		t.Fatalf("stored sessions = %+v (%v), want %s", stored, err, one.id)
	}
	e.mu.Lock()
	turns := e.sessions["ctx"].turns
	e.mu.Unlock()
	if turns != 2 {
		t.Fatalf("turns counted on the session = %d, want 2", turns)
	}
}

// A turn that waited on a creation that failed creates the session itself.
func TestWaitingTurnCreatesTheSessionAfterAFailedCreate(t *testing.T) {
	fake := newFake(nil)
	fake.createErr = errors.New("CMA unavailable")
	g := newGate(t)
	holdCreateOf(fake, "ctx", g)
	e := &Executor{Backend: fake, Store: newStore(t, nil)}
	ctx := context.Background()

	first := acquireAsync(ctx, e, "ctx")
	g.reached(t)
	second := acquireAsync(ctx, e, "ctx")
	select {
	case r := <-second:
		t.Fatalf("the second acquire returned (%q, %v) before the first creation ended", r.id, r.err)
	case <-time.After(quiet):
	}
	g.release()
	if r := acquired(t, first, "the first acquire"); r.err == nil {
		t.Fatal("the first acquire did not fail with CMA's error")
	}
	r := acquired(t, second, "the second acquire")
	if r.err != nil || r.id == "" || fake.sessionCount() != 1 {
		t.Fatalf("second acquire = (%q, %v), %d sessions; want the session it created", r.id, r.err, fake.sessionCount())
	}
}
