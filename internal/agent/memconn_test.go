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
// silence costs no real time, and log records the order of the calls, streams included.
type memConn struct {
	mu          sync.Mutex
	log         []string
	sends, subs []chan item
	gets        []getResult
}

type getResult struct {
	task *a2a.Task
	err  error
}

func (m *memConn) record(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = append(m.log, s)
}

func (m *memConn) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

func (m *memConn) SendStreamingMessage(ctx context.Context, _ *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return m.stream(ctx, "send", &m.sends)
}

func (m *memConn) SubscribeToTask(ctx context.Context, _ *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return m.stream(ctx, "subscribe", &m.subs)
}

func (m *memConn) stream(ctx context.Context, name string, queue *[]chan item) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		m.mu.Lock()
		var ch chan item
		if len(*queue) > 0 {
			ch, *queue = (*queue)[0], (*queue)[1:]
		}
		m.mu.Unlock()
		m.record(name)
		defer m.record(name + " ended")
		if ch == nil {
			yield(nil, errors.New("memConn: no stream queued for "+name))
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

func (m *memConn) GetTask(context.Context, *a2a.GetTaskRequest) (*a2a.Task, error) {
	m.record("get")
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.gets) == 0 {
		return nil, errors.New("memConn: no GetTask result queued")
	}
	r := m.gets[0]
	if len(m.gets) > 1 {
		m.gets = m.gets[1:]
	}
	return r.task, r.err
}

func (m *memConn) CancelTask(_ context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	m.record("cancel " + string(req.ID))
	return &a2a.Task{ID: req.ID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

// memAgent returns an Agent over m with a 10 s watchdog and a 1 s reconnect pause.
func memAgent(m *memConn, tune func(*Config)) *Agent {
	cfg := Config{InactivityTimeout: 10 * time.Second, ReconnectDelay: time.Second}
	if tune != nil {
		tune(&cfg)
	}
	return New(func(context.Context) (Conn, error) { return m, nil }, nil, cfg)
}

// sendAsync starts a Send and returns its result and events once it ends.
func sendAsync(t *testing.T, a *Agent) (result func() error, events func() []Event) {
	t.Helper()
	h, got := collector()
	done := make(chan error, 1)
	go func() { done <- a.Send(t.Context(), message("go"), h) }()
	return func() error { return <-done }, func() []Event { return *got }
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
