package stubagent_test

import (
	"net/http"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

func TestSendWhileTaskWorkingIsBusy(t *testing.T) {
	_, agent, c := newAgent(t)
	slowTurn(agent)
	agent.Next(stubagent.Text("never"))
	_, _, ctxID := startSlow(c)

	status, asJSON := c.call("SendMessage", msg("m2", "again", ctxID))
	st := c.open("SendStreamingMessage", msg("m3", "again", ctxID))
	asSSE := st.all()

	for label, tc := range map[string]struct {
		got, rec obj
	}{
		"SendMessage":          {asJSON, fixture(t, "busy/02-second-SendMessage.response.json")[0]},
		"SendStreamingMessage": {asSSE[0], fixture(t, "busy/03-second-SendStreamingMessage.response.sse")[0]},
	} {
		expectLike(t, label, tc.got, tc.rec)
		info, _ := dig(tc.got, "error", "data").([]any)
		if dig(tc.got, "error", "code") != float64(-32004) || dig(tc.got, "error", "message") != "resource conflict" ||
			len(info) != 1 || dig(info[0], "reason") != "UNSUPPORTED_OPERATION" {
			t.Errorf("%s while working = %v, want -32004 resource conflict, UNSUPPORTED_OPERATION", label, tc.got)
		}
	}
	if status != http.StatusOK || !st.sse || len(asSSE) != 1 {
		t.Errorf("busy answers: HTTP %d, streaming sse %v with %d events; want HTTP 200 and one SSE event", status, st.sse, len(asSSE))
	}
}

func TestRejectedMessageCanBeResentUnchangedOnceTheTaskEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(s *stubagent.Server, c *client, taskID string)
	}{
		{"cancelled", func(_ *stubagent.Server, c *client, taskID string) { c.call("CancelTask", obj{"id": taskID}) }},
		{"completed", func(s *stubagent.Server, c *client, taskID string) {
			s.Release("go")
			c.open("SubscribeToTask", obj{"id": taskID}).all() // ends with the COMPLETED task
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, agent, c := newAgent(t)
			slowTurn(agent)
			agent.Next(stubagent.Text("second"))
			st, taskID, ctxID := startSlow(c)
			_ = st.resp.Body.Close()
			rejected := msg("m2", "again", ctxID)
			if _, got := c.call("SendMessage", rejected); dig(got, "error", "code") != float64(-32004) {
				t.Fatalf("while working = %v, want -32004", got)
			}

			tc.end(s, c, taskID)
			evs := c.send(rejected)

			id, ctx, state := ids(last(evs))
			if state != completed || ctx != ctxID || id == taskID || taskText(last(evs)) != "second" {
				t.Errorf("resend: task %s context %s state %s text %q; want a new task in %s running the queued turn", id, ctx, state, taskText(last(evs)), ctxID)
			}
		})
	}
}

func TestReplayOfFinishedMessageIsOneSnapshotAndRunsNothing(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("The answer is 4."))
	agent.Next(stubagent.Text("other"))
	first := c.send(msg("m1", "What is 2+2?", ""))
	taskID, ctxID, _ := ids(last(first))

	replay := c.send(msg("m1", "What is 2+2?", ""))

	if got, want := shape(replay), "task:COMPLETED"; got != want {
		t.Fatalf("replay shape = %s, want %s", got, want)
	}
	expectLike(t, "replay", replay[0], fixture(t, "session/02-replay-same-messageid.response.sse")[0])
	if id, ctx, _ := ids(replay[0]); id != taskID || ctx != ctxID || taskText(replay[0]) != "The answer is 4." {
		t.Errorf("replay: task %s context %s text %q, want the first send's %s and %s", id, ctx, taskText(replay[0]), taskID, ctxID)
	}
	next := c.send(msg("m2", "again", ctxID))
	if taskText(last(next)) != "other" {
		t.Errorf("the replay used up a queued turn: next send got %q, want %q", taskText(last(next)), "other")
	}
}

func TestReplayWhileRunningJoinsTheLiveTurn(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	agent.Next(stubagent.Text("other"))
	st, taskID, ctxID := startSlow(c)
	_ = st.resp.Body.Close()

	replay := c.open("SendStreamingMessage", msg("m1", "go slowly", ""))
	snap, _ := replay.next() // the replay opens with a snapshot while the turn is held
	s.Release("go")
	evs := append([]obj{snap}, replay.all()...)

	if got, want := shape(evs), "task:WORKING, artifact+append, artifact+last, task:COMPLETED"; got != want {
		t.Fatalf("replay shape = %s, want %s", got, want)
	}
	expectLike(t, "replay snapshot", evs[0], fixture(t, "inflight/02-a-replay-while-running.response.sse")[0])
	id, ctx, _ := ids(evs[0])
	hist, _ := dig(last(evs), "result", "task", "history").([]any)
	arts, _ := dig(last(evs), "result", "task", "artifacts").([]any)
	if id != taskID || ctx != ctxID || taskText(evs[0]) != "tick 1 " || len(hist) != 1 || len(arts) != 1 {
		t.Errorf("replay: task %s context %s text %q, %d messages, %d artifacts; want the running task, one message and one artifact",
			id, ctx, taskText(evs[0]), len(hist), len(arts))
	}
	if got := taskText(last(c.send(msg("m2", "again", ctxID)))); got != "other" {
		t.Errorf("the replay used up a queued turn: next send got %q, want %q", got, "other")
	}
}

// TestReplayCarryingAContextIDIsNotDeduplicated: WP-03 measured (oidc-replay, spec 8.2) that a
// replay with a contextId is not the WP-02 replay without one: while the task runs it is
// refused as busy, and after the task ended it starts a second task in the context, a
// duplicate run of the turn.
func TestReplayCarryingAContextIDIsNotDeduplicated(t *testing.T) {
	s, agent, c := newAgent(t)
	slowTurn(agent)
	agent.Next(stubagent.Text("again"))
	st, taskID, ctxID := startSlow(c)
	_ = st.resp.Body.Close()

	_, running := c.call("SendStreamingMessage", msg("m1", "go slowly", ctxID))
	s.Release("go")
	if err := s.WaitTurn(t.Context(), taskID); err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	evs := c.send(msg("m1", "go slowly", ctxID))

	info, _ := dig(running, "error", "data").([]any)
	if dig(running, "error", "code") != float64(-32004) || dig(running, "error", "message") != "resource conflict" || len(info) != 1 || dig(info[0], "reason") != "UNSUPPORTED_OPERATION" {
		t.Errorf("replay with a contextId while running = %v, want -32004 resource conflict, UNSUPPORTED_OPERATION", running)
	}
	id, ctx, state := ids(last(evs))
	if id == taskID || ctx != ctxID || state != completed || taskText(last(evs)) != "again" {
		t.Errorf("replay with a contextId after the task ended: task %s in %s, %s, text %q; want a new task in %s running the turn again", id, ctx, state, taskText(last(evs)), ctxID)
	}
}

func TestToolCallAndResultAreDataArtifacts(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(stubagent.Text("a "), stubagent.Tool("add_numbers", obj{"a": 3, "b": 5}, obj{"output": "8"}), stubagent.Text("b "))

	evs := c.send(msg("m1", "add", ""))

	if got, want := shape(evs[3:]), "artifact, artifact+last x3, artifact, artifact+last, task:COMPLETED"; got != want {
		t.Fatalf("shape = %s, want %s", got, want)
	}
	expectLike(t, "function_call", evs[5], fixture(t, "approval/01-pause-approve.response.sse")[3])
	expectLike(t, "function_response", evs[6], fixture(t, "approval/02-approve.response.sse")[3])
	call, result := firstPart(evs[5]), firstPart(evs[6])
	if dig(call, "data", "name") != "add_numbers" || dig(call, "data", "args", "a") != float64(3) ||
		dig(call, "metadata", "kagent.dev/a2a/part-type") != "function_call" || dig(result, "data", "id") != dig(call, "data", "id") ||
		dig(result, "data", "response", "output") != "8" || dig(result, "metadata", "kagent.dev/a2a/part-type") != "function_response" {
		t.Errorf("call %v and result %v, want add_numbers(a=3) and its output, sharing one call id", call, result)
	}
	if arts, _ := dig(last(evs), "result", "task", "artifacts").([]any); len(arts) != 4 || taskText(last(evs)) != "a b " {
		t.Errorf("final task has %d artifacts with text %q, want 4 (text, call, result, text) and %q", len(arts), taskText(last(evs)), "a b ")
	}
}

// firstPart returns the first part of an artifactUpdate's artifact.
func firstPart(ev obj) any {
	parts, _ := dig(ev, "result", "artifactUpdate", "artifact", "parts").([]any)
	if len(parts) == 0 {
		return nil
	}
	return parts[0]
}
