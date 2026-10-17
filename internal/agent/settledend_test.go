package agent

import (
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

func status(state a2a.TaskState) a2a.Event {
	return &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: state}}
}

// reconnects is how many reconnect attempts h counted, whatever their outcome.
func reconnects(t *testing.T, h *telemetrytest.Harness) float64 {
	t.Helper()
	n := 0.0
	for _, outcome := range []string{"success", "error", "timeout", "cancelled"} {
		n += h.Counter(t, "agentkourier_stream_reconnects_total", map[string]string{"outcome": outcome})
	}
	return n
}

// A stream may end on a status update that settles the task instead of on the full Task, as
// the a2a-sdk Python server always ends one. When it ends cleanly so, GetTask, at once, gives
// the final task: no reconnect pause and no reconnect counted. A cut stream, or a clean end
// in a state that has not settled, is a drop as before: GetTask after ReconnectDelay.
func TestStreamEnd(t *testing.T) {
	const pause, now = time.Second, time.Duration(0)
	settledEnd := func(state a2a.TaskState) []item {
		return []item{{ev: working()}, {ev: delta("ab")}, {ev: status(state)}}
	}
	cases := []struct {
		name       string
		stream     []item // closed after its items unless one is an error
		gets       []getResult
		subscribe  []item
		wantCalls  []string
		wantGetIn  time.Duration // from the end of the send stream to the first GetTask
		wantFinal  a2a.TaskState
		reconnects float64
	}{
		{
			name:      "ends with the task",
			stream:    []item{{ev: working()}, {ev: delta("ab")}, {ev: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls: []string{"send", "send ended"},
			wantFinal: a2a.TaskStateCompleted,
		},
		{
			name:      "clean end after completed",
			stream:    settledEnd(a2a.TaskStateCompleted),
			gets:      []getResult{{task: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls: []string{"send", "send ended", "get t1"},
			wantGetIn: now,
			wantFinal: a2a.TaskStateCompleted,
		},
		{
			name:      "clean end after failed",
			stream:    settledEnd(a2a.TaskStateFailed),
			gets:      []getResult{{task: task(a2a.TaskStateFailed, "ab")}},
			wantCalls: []string{"send", "send ended", "get t1"},
			wantGetIn: now,
			wantFinal: a2a.TaskStateFailed,
		},
		{
			name:      "clean end after canceled",
			stream:    settledEnd(a2a.TaskStateCanceled),
			gets:      []getResult{{task: task(a2a.TaskStateCanceled, "ab")}},
			wantCalls: []string{"send", "send ended", "get t1"},
			wantGetIn: now,
			wantFinal: a2a.TaskStateCanceled,
		},
		{
			name:      "clean end after input-required",
			stream:    settledEnd(a2a.TaskStateInputRequired),
			gets:      []getResult{{task: task(a2a.TaskStateInputRequired, "ab")}},
			wantCalls: []string{"send", "send ended", "get t1"},
			wantGetIn: now,
			wantFinal: a2a.TaskStateInputRequired,
		},
		{
			name:      "clean end after auth-required",
			stream:    settledEnd(a2a.TaskStateAuthRequired),
			gets:      []getResult{{task: task(a2a.TaskStateAuthRequired, "ab")}},
			wantCalls: []string{"send", "send ended", "get t1"},
			wantGetIn: now,
			wantFinal: a2a.TaskStateAuthRequired,
		},
		{
			name:       "clean end after working",
			stream:     []item{{ev: working()}, {ev: delta("ab")}},
			gets:       []getResult{{task: task(a2a.TaskStateWorking, "ab")}},
			subscribe:  []item{{ev: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls:  []string{"send", "send ended", "get t1", "subscribe t1", "subscribe ended"},
			wantGetIn:  pause,
			wantFinal:  a2a.TaskStateCompleted,
			reconnects: 1,
		},
		{
			name:       "cut after completed",
			stream:     append(settledEnd(a2a.TaskStateCompleted), item{err: io.ErrUnexpectedEOF}),
			gets:       []getResult{{task: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls:  []string{"send", "send ended", "get t1"},
			wantGetIn:  pause,
			wantFinal:  a2a.TaskStateCompleted,
			reconnects: 1,
		},
		{
			name:       "clean end after completed, GetTask fails",
			stream:     settledEnd(a2a.TaskStateCompleted),
			gets:       []getResult{{err: errors.New("down")}, {task: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls:  []string{"send", "send ended", "get t1", "get t1"},
			wantGetIn:  now,
			wantFinal:  a2a.TaskStateCompleted,
			reconnects: 1,
		},
		{
			name:       "clean end after completed, GetTask says working",
			stream:     settledEnd(a2a.TaskStateCompleted),
			gets:       []getResult{{task: task(a2a.TaskStateWorking, "ab")}, {task: task(a2a.TaskStateCompleted, "ab")}},
			wantCalls:  []string{"send", "send ended", "get t1", "get t1"},
			wantGetIn:  now,
			wantFinal:  a2a.TaskStateCompleted,
			reconnects: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := telemetrytest.New(t)
				send := make(chan item)
				var subs []chan item
				if tc.subscribe != nil {
					subs = []chan item{make(chan item)}
				}
				m := newMemConn(t, []chan item{send}, subs, tc.gets...)
				collect, got := collector()
				done := make(chan error, 1)
				go func() { done <- memAgent(t, m, nil).Send(h.Ctx, message("go"), collect) }()
				feedAll := func(ch chan item, items []item) {
					for _, it := range items {
						feed(t, ch, it, done)
					}
					if len(items) == 0 || items[len(items)-1].err == nil {
						close(ch)
					}
				}
				feedAll(send, tc.stream)
				if subs != nil {
					feedAll(subs[0], tc.subscribe)
				}
				var err error
				select {
				case err = <-done:
				case <-time.After(time.Minute):
					t.Fatal("Send never returned")
				}
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
				if calls := m.calls(); !slices.Equal(calls, tc.wantCalls) {
					t.Errorf("calls = %q, want %q", calls, tc.wantCalls)
				}
				if ended, ok := m.at("send ended"); ok {
					if get, ok := m.at("get t1"); ok {
						if d := get.Sub(ended); d != tc.wantGetIn {
							t.Errorf("GetTask came %v after the stream ended, want %v", d, tc.wantGetIn)
						}
					}
				}
				events := *got
				final, ok := events[len(events)-1].(FinalEvent)
				if !ok || final.Task.Status.State != tc.wantFinal {
					t.Errorf("last event = %#v, want a FinalEvent in %s", events[len(events)-1], tc.wantFinal)
				}
				if finals := countEvents[FinalEvent](events); finals != 1 {
					t.Errorf("%d FinalEvents, want 1", finals)
				}
				if texts := texts(events); !slices.Equal(texts, []string{"ab"}) {
					t.Errorf("texts = %q, want the text once", texts)
				}
				if n := reconnects(t, h); n != tc.reconnects {
					t.Errorf("%v reconnects counted, want %v", n, tc.reconnects)
				}
			})
		})
	}
}

// Follow reads a subscribe stream the same way: one that ends cleanly on a settling status
// update is settled by GetTask at once, not by a reconnect.
func TestFollowSettlesAStreamThatEndsOnAStatusUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := telemetrytest.New(t)
		sub := make(chan item)
		m := newMemConn(t, nil, []chan item{sub},
			getResult{task: task(a2a.TaskStateWorking, "a")},
			getResult{task: task(a2a.TaskStateCompleted, "ab")})
		collect, got := collector()
		done := make(chan error, 1)
		go func() { done <- memAgent(t, m, nil).Follow(h.Ctx, "t1", collect) }()
		feed(t, sub, item{ev: delta("b")}, done)
		feed(t, sub, item{ev: status(a2a.TaskStateCompleted)}, done)
		close(sub)
		if err := <-done; err != nil {
			t.Fatalf("Follow: %v", err)
		}
		want := []string{"get t1", "subscribe t1", "subscribe ended", "get t1"}
		if calls := m.calls(); !slices.Equal(calls, want) {
			t.Errorf("calls = %q, want %q", calls, want)
		}
		events := *got
		if final, ok := events[len(events)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateCompleted {
			t.Errorf("last event = %#v, want the COMPLETED task", events[len(events)-1])
		}
		if texts := texts(events); !slices.Equal(texts, []string{"a", "ab"}) {
			t.Errorf("texts = %q, want a then ab", texts)
		}
		if n := reconnects(t, h); n != 0 {
			t.Errorf("%v reconnects counted, want none", n)
		}
	})
}

// countEvents is how many of events are of type E.
func countEvents[E Event](events []Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(E); ok {
			n++
		}
	}
	return n
}
