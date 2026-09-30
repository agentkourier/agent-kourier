package agent

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// item is one step of a stream a test feeds by hand: an event, or an error that ends it.
type item struct {
	ev  a2a.Event
	err error
}

// memConn is a Conn with no network. Each stream call takes the next channel a test queued
// and yields what the test sends on it; closing it ends the stream cleanly. GetTask returns
// the queued gets in order and repeats the last. Tests run inside a synctest bubble, so
// silence costs no real time. A call the test did not script fails the test. log records
// the calls in order, with the task ids the requests named ("get t1", "subscribe t1",
// "cancel t1"), and each stream's end ("send ended"); at gives when each happened.
type memConn struct {
	t           *testing.T
	mu          sync.Mutex
	log         []string
	times       []time.Time
	reported    bool // an unscripted call has been reported
	sends, subs []chan item
	gets        []getResult
	onCancel    func() // runs at the start of CancelTask, for a test to cancel a context at that moment
	cancelErr   error  // when set, CancelTask fails with it
	list        func(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error)
}

// newMemConn returns a memConn that fails t on an unscripted call. A test that needs list or
// onCancel sets them on the result.
func newMemConn(t *testing.T, sends, subs []chan item, gets ...getResult) *memConn {
	return &memConn{t: t, sends: sends, subs: subs, gets: gets}
}

type getResult struct {
	task *a2a.Task
	err  error
}

func (m *memConn) record(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = append(m.log, s)
	m.times = append(m.times, time.Now())
}

func (m *memConn) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

// at returns when the first call or stream end named entry was recorded.
// It reports false if there is no such entry, which a test that already failed can hit.
func (m *memConn) at(entry string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.log {
		if e == entry {
			return m.times[i], true
		}
	}
	return time.Time{}, false
}

// unscripted fails the test for a call it did not expect, and returns the error to hand
// back. Only the first is reported: a runaway loop would print thousands.
func (m *memConn) unscripted(call string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.reported {
		m.reported = true
		m.t.Errorf("memConn: unscripted call %s", call)
	}
	return errors.New("memConn: unscripted call " + call)
}

func (m *memConn) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	m.record("list")
	if m.list == nil {
		return nil, m.unscripted("list")
	}
	return m.list(ctx, req)
}

func (m *memConn) SendStreamingMessage(ctx context.Context, _ *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return m.stream(ctx, "send", "send", &m.sends)
}

func (m *memConn) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return m.stream(ctx, "subscribe", "subscribe "+string(req.ID), &m.subs)
}

func (m *memConn) stream(ctx context.Context, name, entry string, queue *[]chan item) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		m.mu.Lock()
		var ch chan item
		if len(*queue) > 0 {
			ch, *queue = (*queue)[0], (*queue)[1:]
		}
		m.mu.Unlock()
		m.record(entry)
		defer m.record(name + " ended")
		if ch == nil {
			yield(nil, m.unscripted(entry))
			return
		}
		for {
			select {
			case it, ok := <-ch:
				if !ok || !yield(it.ev, it.err) || it.err != nil {
					return
				}
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
	}
}

func (m *memConn) GetTask(_ context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	m.record("get " + string(req.ID))
	m.mu.Lock()
	if len(m.gets) == 0 {
		m.mu.Unlock()
		return nil, m.unscripted("get " + string(req.ID))
	}
	defer m.mu.Unlock()
	r := m.gets[0]
	if len(m.gets) > 1 {
		m.gets = m.gets[1:]
	}
	return r.task, r.err
}

func (m *memConn) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if m.onCancel != nil {
		m.onCancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.record("cancel " + string(req.ID))
	if m.cancelErr != nil {
		return nil, m.cancelErr
	}
	return &a2a.Task{ID: req.ID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

// memAgent returns an Agent over m with a 10 s watchdog and a 1 s reconnect pause.
func memAgent(t *testing.T, m *memConn, tune func(*Config)) *Agent {
	t.Helper()
	cfg := Config{InactivityTimeout: 10 * time.Second, ReconnectDelay: time.Second}
	if tune != nil {
		tune(&cfg)
	}
	return New(func(context.Context) (Conn, error) { return m, nil }, nil, cfg)
}

// sending is a Send running in a goroutine of the test's synctest bubble.
type sending struct {
	t    *testing.T
	done chan error
	got  *[]Event
}

func sendAsync(t *testing.T, a *Agent) *sending {
	t.Helper()
	h, got := collector()
	s := &sending{t: t, done: make(chan error, 1), got: got}
	go func() { s.done <- a.Send(t.Context(), message("go"), h) }()
	return s
}

// feed sends what a stream is to yield next. It fails the test if Send has already
// returned, so a regression that ends the send early is an assertion and not a deadlock.
func (s *sending) feed(ch chan<- item, it item) { feed(s.t, ch, it, s.done) }

// result waits for Send to return. A minute on the fake clock is far longer than any test
// needs, so a Send that never returns fails the test fast, on a runaway loop or a hang.
func (s *sending) result() error {
	s.t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(time.Minute):
		s.t.Fatalf("Send never returned")
		return nil
	}
}

func (s *sending) events() []Event { return *s.got }

// feed sends it on ch, or fails the test if the send under test finished first, or never
// read the stream: a minute on the fake clock is far longer than any test needs.
func feed(t *testing.T, ch chan<- item, it item, done <-chan error) {
	t.Helper()
	select {
	case ch <- it:
	case err := <-done:
		t.Fatalf("the send returned (%v) while the test was still feeding its stream", err)
	case <-time.After(time.Minute):
		t.Fatalf("the stream was never read")
	}
}

// The events below name task t1 in context c1.
func working() a2a.Event {
	return &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

func delta(text string) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Append: true, Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
}

// task is a snapshot of t1 in the given state holding text in artifact a.
func task(state a2a.TaskState, text string) *a2a.Task {
	tk := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: state}}
	if text != "" {
		tk.Artifacts = []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
	}
	return tk
}
