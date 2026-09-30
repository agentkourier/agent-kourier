package stubagent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

const (
	working  = "TASK_STATE_WORKING"
	canceled = "TASK_STATE_CANCELED"
)

// slowTurn scripts a turn that streams "tick 1 ", waits at the gate "go", then streams "tick 2 ".
func slowTurn(agent *stubagent.Agent) {
	agent.Next(stubagent.Text("tick 1 "), stubagent.Hold("go"), stubagent.Text("tick 2 "))
}

// startSlow sends into a slow turn and returns its open stream, read up to the first text chunk,
// with the task and context ids.
func startSlow(c *client) (st *stream, taskID, ctxID string) {
	c.t.Helper()
	st = c.open("SendStreamingMessage", msg("m1", "go slowly", ""))
	for ev, ok := st.next(); ok; ev, ok = st.next() {
		taskID, ctxID, _ = ids(ev)
		if kind, _ := body(ev); kind == "artifactUpdate" {
			return st, taskID, ctxID
		}
	}
	c.t.Fatalf("the stream ended before the first text chunk")
	return nil, "", ""
}

func TestGetTaskDuringTurnShowsTextSoFar(t *testing.T) {
	_, agent, c := newAgent(t)
	slowTurn(agent)
	st, taskID, ctxID := startSlow(c)
	_ = st.resp.Body.Close() // drop the stream

	status, got := c.call("GetTask", obj{"id": taskID})

	if id, ctx, state := ids(got); status != 200 || id != taskID || ctx != ctxID || state != working {
		t.Errorf("GetTask = task %s context %s state %s, want %s, %s, %s", id, ctx, state, taskID, ctxID, working)
	}
	if text := taskText(got); text != "tick 1 " {
		t.Errorf("text so far = %q, want %q", text, "tick 1 ")
	}
	if kind, _ := body(got); kind != "task" || dig(got, "result", "history") == nil || dig(got, "result", "artifacts") == nil {
		t.Errorf("GetTask result = %v, want the task itself under result", got["result"])
	}
}

func TestDroppedStreamDoesNotStopTheTurn(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	st, taskID, _ := startSlow(c)
	_ = st.resp.Body.Close()

	s.Release("go")
	if err := s.WaitTurn(t.Context(), taskID); err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}

	_, got := c.call("GetTask", obj{"id": taskID})
	if _, _, state := ids(got); state != completed || taskText(got) != "tick 1 tick 2 " {
		t.Errorf("after the stream was dropped: state %s, text %q, want %s and both ticks", state, taskText(got), completed)
	}
}

func TestWaitTurn(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	st, taskID, _ := startSlow(c)
	_ = st.resp.Body.Close()

	held, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	waiting := s.WaitTurn(held, taskID) // the turn is at the gate
	s.Release("go")
	released := s.WaitTurn(t.Context(), taskID)
	unknown := s.WaitTurn(t.Context(), "nope")

	if !errors.Is(waiting, context.DeadlineExceeded) || released != nil || unknown == nil {
		t.Errorf("WaitTurn while held = %v, after release = %v, unknown task = %v; want a deadline, nil and an error", waiting, released, unknown)
	}
}

func TestSubscribeToTaskJoinsTheRunningTurn(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	st, taskID, ctxID := startSlow(c)
	_ = st.resp.Body.Close()

	sub := c.open("SubscribeToTask", obj{"id": taskID})
	snap, _ := sub.next() // the snapshot arrives while the turn is held
	s.Release("go")
	evs := append([]obj{snap}, sub.all()...)

	if got, want := shape(evs), "task:WORKING, artifact+append, artifact+last, task:COMPLETED"; got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	if id, ctx, _ := ids(evs[0]); id != taskID || ctx != ctxID || taskText(evs[0]) != "tick 1 " {
		t.Errorf("snapshot: task %s context %s text %q, want %s, %s and the text so far", id, ctx, taskText(evs[0]), taskID, ctxID)
	}
	if got := assemble(evs); got != "tick 1 tick 2 " {
		t.Errorf("snapshot plus deltas = %q, want %q", got, "tick 1 tick 2 ")
	}
	rec := fixture(t, "reconnect/03-subscribe.response.sse")
	expectLike(t, "snapshot", evs[0], rec[0])
	expectLike(t, "final task", last(evs), last(rec))
}

func TestSubscribeToFinishedTaskIsOneSnapshot(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("done"))
	_, sent := c.call("SendMessage", msg("m1", "hi", ""))

	st := c.open("SubscribeToTask", obj{"id": task(t, sent)["id"]})
	evs := st.all()

	if !st.sse || len(evs) != 1 || describe(evs[0]) != "task:COMPLETED" || taskText(evs[0]) != "done" {
		t.Errorf("SubscribeToTask after the end = sse %v, %d events, %v; want one COMPLETED snapshot", st.sse, len(evs), evs)
	}
}

func TestSubscribeToUnknownTaskIsOneErrorEvent(t *testing.T) {
	_, _, c := newAgent(t)

	_, got := c.call("SubscribeToTask", obj{"id": "nope"})

	if dig(got, "error", "code") != float64(-32001) {
		t.Errorf("SubscribeToTask nope = %v, want error -32001", got)
	}
}

func TestGetTaskUnknownIDIsNotFound(t *testing.T) {
	_, _, c := newAgent(t)

	_, got := c.call("GetTask", obj{"id": "nope"})
	_, none := c.call("GetTask", obj{})

	if dig(got, "error", "code") != float64(-32001) || dig(none, "error", "code") != float64(-32602) {
		t.Errorf("GetTask nope = %v, GetTask with no id = %v; want -32001 and -32602", got, none)
	}
}

func TestCancelTaskStopsTheTurn(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	st, taskID, _ := startSlow(c)

	status, got := c.call("CancelTask", obj{"id": taskID})

	if _, _, state := ids(got); status != 200 || state != canceled || taskText(got) != "tick 1 " {
		t.Errorf("CancelTask = HTTP %d, state %s, text %q; want %s with the text so far", status, state, taskText(got), canceled)
	}
	rest := st.all() // the open stream ends with the CANCELED task
	if describe(last(rest)) != "task:CANCELED" {
		t.Errorf("open stream ended with %s, want task:CANCELED", describe(last(rest)))
	}
	s.Release("go")
	if err := s.WaitTurn(t.Context(), taskID); err != nil { // the turn has ended, so it has said all it will
		t.Fatalf("WaitTurn: %v", err)
	}
	_, again := c.call("GetTask", obj{"id": taskID})
	if _, _, state := ids(again); state != canceled || taskText(again) != "tick 1 " {
		t.Errorf("after the gate opened: state %s, text %q, want %s and %q", state, taskText(again), canceled, "tick 1 ")
	}
}

func TestCancelTaskRefusesAFinishedTask(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	_, sent := c.call("SendMessage", msg("m1", "hi", ""))

	_, got := c.call("CancelTask", obj{"id": task(t, sent)["id"]})

	if dig(got, "error", "code") != float64(-32002) {
		t.Errorf("CancelTask on a completed task = %v, want error -32002", got)
	}
}
