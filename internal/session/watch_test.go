package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// reopening is a watcher that records each answer it is told of, and what the manager had parked when it was told.
type reopening struct {
	m    *Manager
	mu   sync.Mutex
	got  []string // "task answerID"
	held []bool   // whether the runner was parked on the task, and not released, as the call was made
	told bool     // what it answers: it tells the thread itself
}

func (w *reopening) AnswerReopened(_ context.Context, k Key, task a2a.TaskID, answerID string) bool {
	if k != key {
		return false
	}
	w.m.mu.Lock()
	c := w.m.convos[k]
	parked := c != nil && c.parked == task && !c.released
	w.m.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got, w.held = append(w.got, string(task)+" "+answerID), append(w.held, parked)
	return w.told
}

func (w *reopening) calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got...)
}

// watched is pausedFirst with a watcher on the manager, set before any answer is sent.
func watched(t *testing.T, told bool, answer func(*a2a.Message, agent.Handler) error) (*fixture, *fakeSink, *reopening) {
	t.Helper()
	w := &reopening{told: told}
	f, sink := pausedFirst(t, answer)
	w.m = f.m
	f.m.mu.Lock()
	f.m.cfg.AnswerWatcher = w
	f.m.mu.Unlock()
	return f, sink, w
}

// An answer the agent did not take reopens the pause, and the watcher is told once, with the answer's ID, after the runner is
// parked on the task again: a Cancel or an Answer for it is accepted from then. The manager's notice is its own unless the
// watcher says it will tell the thread.
func TestAnAnswerThatWasNotDeliveredIsToldToTheWatcher(t *testing.T) {
	down := func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: down", agent.ErrNotSent) }
	for name, told := range map[string]bool{"the manager tells the thread": false, "the watcher tells the thread": true} {
		t.Run(name, func(t *testing.T) {
			f, sink, w := watched(t, told, down)
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the watcher to be told", func() bool { return len(w.calls()) == 1 })
			eventually(t, "the pause to reopen", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
			w.mu.Lock()
			if w.got[0] != "t1 ans-1" || !w.held[0] {
				t.Errorf("told %q with the runner parked %v; want the answer's ID, with the runner parked on the task", w.got, w.held)
			}
			w.mu.Unlock()
			// The pause is open: it takes a Cancel, which the watcher's caller would make.
			if err := f.m.Cancel(t.Context(), key, "t1", "the answer was not delivered"); err != nil {
				t.Fatalf("Cancel after the watcher was told = %v; want the reopened pause cancelled", err)
			}
			time.Sleep(50 * time.Millisecond) // a notice, if one is coming, has come
			if got := len(sink.noticesOf(NoticeFailed)); (got == 1) == told || got > 1 {
				t.Errorf("%d failed notices with told = %v; want the manager's own one only when the watcher does not tell the thread", got, told)
			}
			if got := w.calls(); len(got) != 1 {
				t.Errorf("told %q; want once", got)
			}
		})
	}
}

// Which outcomes of an answer reopen the pause (and so tell the watcher): the ones that leave the task waiting for input. A
// rejected answer cancelled its task, an ended task dropped the answer, and a working one is followed.
func TestOnlyAnAnswerThatLeavesThePauseOpenIsToldToTheWatcher(t *testing.T) {
	waiting := func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &t0}}, nil
	}
	ended := func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
	}
	for _, tc := range []struct {
		name   string
		answer func(*a2a.Message, agent.Handler) error
		get    func(a2a.TaskID) (*a2a.Task, error)
		told   bool
	}{
		{"the send failed", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: down", agent.ErrNotSent) }, working(t0), true},
		{"the outcome is unknown and the task waits", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: cut", agent.ErrOutcomeUnknown) }, waiting, true},
		{"the agent was busy and the task waits", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: busy", agent.ErrBusy) }, waiting, true},
		{"the outcome is unknown and the task runs", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: cut", agent.ErrOutcomeUnknown) }, working(t0), false},
		{"the outcome is unknown and the task ended", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: cut", agent.ErrOutcomeUnknown) }, ended, false},
		{"the agent rejected it", func(*a2a.Message, agent.Handler) error { return fmt.Errorf("%w: no", agent.ErrAnswerRejected) }, working(t0), false},
		{"the agent took it", func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }, working(t0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, w := watched(t, false, tc.answer)
			f.agent.mu.Lock()
			f.agent.get = tc.get
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) }
			f.agent.mu.Unlock()
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			if tc.told {
				eventually(t, "the watcher to be told", func() bool { return len(w.calls()) == 1 })
				return
			}
			// The task ends or is followed to its end, and the held reply goes out: the answer has been dealt with.
			if id, want := f.nextSent(), inbound("2").MessageID; id != want {
				t.Errorf("sent %s; want the held reply %s", id, want)
			}
			if got := w.calls(); len(got) != 0 {
				t.Errorf("told %q; want the watcher left alone: the pause did not reopen", got)
			}
		})
	}
}
