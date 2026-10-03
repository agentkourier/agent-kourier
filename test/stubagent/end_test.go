package stubagent_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// statusText is the text of a task event's status message, "" if it has none.
func statusText(ev obj) string {
	_, e := body(ev)
	return text(dig(e, "status", "message", "parts"))
}

// Fail and Reject end the turn with the task in that state and the reason as its status message, as an
// agent whose model call failed does. Text said before them stays in the task's output; steps after them
// do not run.
func TestFailAndRejectEndTheTurnInThatState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []stubagent.Step
		state string
		why   string // the status message
		text  string // the output the task keeps
	}{
		{"failed with a reason and no output", []stubagent.Step{stubagent.Fail("model call failed: HTTP 401")}, "TASK_STATE_FAILED", "model call failed: HTTP 401", ""},
		{"failed after some output", []stubagent.Step{stubagent.Text("partial"), stubagent.Fail("boom"), stubagent.Text("never said")}, "TASK_STATE_FAILED", "boom", "partial"},
		{"rejected", []stubagent.Step{stubagent.Reject("not something I do")}, "TASK_STATE_REJECTED", "not something I do", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, agent, c := newAgent(t)
			agent.Next(tc.steps...)

			evs := c.send(msg("m1", "hi", ""))

			final := last(evs)
			taskID, _, state := ids(final)
			if kind, _ := body(final); kind != "task" || state != tc.state {
				t.Fatalf("the stream ends with %s in state %s; want the task in %s", kind, state, tc.state)
			}
			if got := statusText(final); got != tc.why {
				t.Errorf("status message = %q; want %q", got, tc.why)
			}
			if got := taskText(final); got != tc.text {
				t.Errorf("task output = %q; want %q", got, tc.text)
			}
			// GetTask shows the same end, and the task cannot be cancelled any more.
			_, got := c.call("GetTask", obj{"id": taskID})
			if _, _, state := ids(got); state != tc.state || statusText(got) != tc.why {
				t.Errorf("GetTask = state %s, status message %q; want %s and %q", state, statusText(got), tc.state, tc.why)
			}
			if _, got := c.call("CancelTask", obj{"id": taskID}); dig(got, "error") == nil {
				t.Errorf("CancelTask of a %s task = %v; want it refused", tc.state, got)
			}
		})
	}
}
