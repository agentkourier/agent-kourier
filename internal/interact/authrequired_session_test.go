package interact_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// TestAnAuthPauseIsCancelledAndTheReplyBehindItGoesOut: a task that asks for authentication (auth-required) cannot be answered
// from the thread, so it is turned away as an unusable pause: the thread is told, the task is cancelled, and the reply queued
// behind it is sent. Without that, the runner stays parked on the task, which has no interaction, so no prompt and no timeout,
// and nothing ends it: the reply behind it would wait for ever.
func TestAnAuthPauseIsCancelledAndTheReplyBehindItGoesOut(t *testing.T) {
	ag := newScriptedAgent()
	ag.sendAt = func(n int) a2a.Task {
		if n == 0 {
			return a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateAuthRequired, Timestamp: &t0, Message: &a2a.Message{ID: "auth-1"}}}
		}
		return a2a.Task{ID: "task-2", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: &t0}}
	}
	p := start(t, openStore(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	if _, err := p.mgr.Submit(t.Context(), session.Inbound{Key: key, Binding: "ns/b", MessageID: "slack:main:C1:1.2", MessageRef: "1.2", UserID: "U1", Text: "hello", Create: true}); err != nil {
		t.Fatal(err)
	}
	until(t, "the first turn to be sent", func() bool { return ag.sent() == 1 })
	if _, err := p.mgr.Submit(t.Context(), session.Inbound{Key: key, Binding: "ns/b", MessageID: "slack:main:C1:1.3", MessageRef: "1.3", UserID: "U1", Text: "and now?"}); err != nil {
		t.Fatal(err)
	}

	until(t, "the reply behind the auth pause to be sent", func() bool { return ag.sent() == 2 })
	if got := ag.cancelled(); !slices.Equal(got, []a2a.TaskID{"task-1"}) {
		t.Errorf("the agent was asked to cancel %v; want the auth-required task, once", got)
	}
	if got := strings.Join(p.notices(), "\n"); strings.Count(got, "cannot use") != 1 {
		t.Errorf("notices %q; want the unusable-pause notice, once", got)
	}
	if p.mgr.HoldsTask(key, "task-1") {
		t.Error("the session still holds the auth-required task")
	}
}
