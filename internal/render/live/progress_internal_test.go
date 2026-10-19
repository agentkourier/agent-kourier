package live

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// These tests drive a Run by hand, with no goroutine and a clock they turn: the state of the progress modes after an event
// is what they look at, and a case that wants a card sent, or not, says so, since a snapshot is the goroutine's.

type handClock struct{ t time.Time }

func (c *handClock) now() time.Time { return c.t }

// newTimedRun is a Run in a progress mode that keeps time, as Start makes one, on a stream with steps or on the update path.
func newTimedRun(streaming bool, mode v1alpha1.ProgressMode) (*Run, *handClock) {
	clk := &handClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	u := newRun(streaming, true)
	u.cfg = Config{ActivityDelay: 3 * time.Second, Now: clk.now}
	u.progress, u.timed = mode, true
	u.started, u.gapAt, u.gapOpen, u.gapSeq, u.calls = clk.t, clk.t, true, 1, map[string]bool{}
	return u, clk
}

// pass turns the clock d and lets the delay timer of the open gap fire, as the run's goroutine does.
func (c *handClock) pass(u *Run, d time.Duration) {
	c.t = c.t.Add(d)
	u.openIdle(u.gapSeq, c.t)
}

func finalOf(state a2a.TaskState) agent.FinalEvent {
	return agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: state}}}
}

// The idle card of a turn that ends with nothing to show is no content: no stream is made for a card that was never sent, and
// the turn ends as it did before the card, with no output.
func TestAnUnclaimedWorkingCardThatWasNeverSentIsNoContentAtTheFinal(t *testing.T) {
	for _, mode := range []v1alpha1.ProgressMode{v1alpha1.ProgressStepsAndActivity, v1alpha1.ProgressActivity} {
		for name, final := range map[string]agent.FinalEvent{
			"completed": {}, "failed": finalOf(a2a.TaskStateFailed), "canceled": finalOf(a2a.TaskStateCanceled), "rejected": finalOf(a2a.TaskStateRejected),
		} {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				u, clk := newTimedRun(true, mode)
				clk.pass(u, 3*time.Second)
				if v := u.snapshot(); !v.ok || len(v.steps) != 1 {
					t.Fatalf("before the end: ok %v, %d steps; want the card shown", v.ok, len(v.steps))
				}
				u.Handle(final)
				if v := u.snapshot(); v.ok || len(v.steps) != 0 || !v.final {
					t.Errorf("at the end: ok %v, %d steps; want nothing to show, so that no stream is made for a card", v.ok, len(v.steps))
				}
			})
		}
	}
}

// A card that was sent is ended, by how the task ended: a tick for a completed task, and for a pause, and an error for a task
// that did not finish.
func TestACardThatWasSentIsEndedByHowTheTaskEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final agent.FinalEvent
		title string
		state chat.StepState
	}{
		{"completed", agent.FinalEvent{}, "Finished", chat.StepDone},
		{"completed task", finalOf(a2a.TaskStateCompleted), "Finished", chat.StepDone},
		{"input required", finalOf(a2a.TaskStateInputRequired), "Waiting for your answer", chat.StepDone},
		{"auth required", finalOf(a2a.TaskStateAuthRequired), "Waiting for your answer", chat.StepDone},
		{"failed", finalOf(a2a.TaskStateFailed), "Stopped", chat.StepFailed},
		{"canceled", finalOf(a2a.TaskStateCanceled), "Stopped", chat.StepFailed},
		{"rejected", finalOf(a2a.TaskStateRejected), "Stopped", chat.StepFailed},
	} {
		t.Run("idle/"+tc.name, func(t *testing.T) {
			u, clk := newTimedRun(true, v1alpha1.ProgressStepsAndActivity)
			clk.pass(u, 3*time.Second)
			u.sentStep(render.WorkingStep("s1"))
			u.Handle(tc.final)
			v := u.snapshot()
			if !v.ok || len(v.steps) != 1 || v.steps[0].Title != tc.title || v.steps[0].State != tc.state {
				t.Errorf("steps %+v; want %q in state %v", v.steps, tc.title, tc.state)
			}
		})
		t.Run("activity/"+tc.name, func(t *testing.T) {
			u, clk := newTimedRun(true, v1alpha1.ProgressActivity)
			u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
			clk.t = clk.t.Add(4 * time.Second)
			u.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"output": "x"}})
			u.sentStep(render.ActivityStep("s1", 0, 0, 0, render.NotEnded))
			u.Handle(tc.final)
			v := u.snapshot()
			want := tc.title
			if tc.title == "Finished" {
				want = "Worked"
			}
			if !v.ok || len(v.steps) != 1 || v.steps[0].Title != want || v.steps[0].State != tc.state || v.steps[0].Detail != "Took 4 s, 1 step" {
				t.Errorf("steps %+v; want %q in state %v with the summary", v.steps, want, tc.state)
			}
		})
	}
}

// Only text that came while a card was open says that it wrote the answer. A gap that opens after all the text was written, and
// is ended by the final, did not write anything.
func TestAnIdleCardSaysItWroteTheAnswerOnlyForTextThatCameWhileItWasOpen(t *testing.T) {
	u, clk := newTimedRun(true, v1alpha1.ProgressStepsAndActivity)
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "Checking."})
	clk.pass(u, 3*time.Second)
	u.sentStep(render.WorkingStep("s1"))
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "Checking. Three pods."})
	if v := u.snapshot(); v.steps[0].Title != "Wrote the answer" {
		t.Fatalf("title %q after text came to the open card; want Wrote the answer", v.steps[0].Title)
	}
	// Another pause, ended by the final with no text since.
	clk.pass(u, 3*time.Second)
	u.sentStep(render.WorkingStep("s2"))
	u.Handle(agent.FinalEvent{})
	v := u.snapshot()
	if len(v.steps) != 2 || v.steps[1].Title != "Finished" {
		t.Errorf("steps %+v; want the card the final ended to say Finished, not that it wrote anything", v.steps)
	}
}

// A call with no ID can never have a result, so it is no gap while it is the latest event, and it is counted.
func TestACallWithNoIDIsUnderWayUntilTheNextEventAndCounted(t *testing.T) {
	u, clk := newTimedRun(true, v1alpha1.ProgressStepsAndActivity)
	u.Handle(agent.ToolCallEvent{Name: "search"})
	if u.gapOpen {
		t.Fatal("the gap is open beside a call with no ID, which has no result to wait for")
	}
	clk.pass(u, 10*time.Second)
	if v := u.snapshot(); len(v.steps) != 1 {
		t.Errorf("%d steps after a long wait beside a call with no ID; want its own card alone", len(v.steps))
	}
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "Done."})
	if !u.gapOpen {
		t.Error("the gap did not open at the event after the call")
	}

	act, _ := newTimedRun(false, v1alpha1.ProgressActivity)
	act.Handle(agent.ToolCallEvent{Name: "search"})
	act.Handle(agent.ToolCallEvent{Name: "fetch"})
	if act.finished != 2 {
		t.Errorf("%d steps counted for two calls with no ID; want both", act.finished)
	}
}

// A call that is reported again changes nothing: not the line, not the gap, not what was counted.
func TestACallReportedAgainChangesNothing(t *testing.T) {
	u, clk := newTimedRun(false, v1alpha1.ProgressStepsAndActivity)
	call := agent.ToolCallEvent{CallID: "c1", Name: "search"}
	u.Handle(call)
	u.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"output": "x"}})
	clk.pass(u, 3*time.Second)
	if !u.idle {
		t.Fatal("Working… is not showing after a gap")
	}
	seq, finished := u.gapSeq, u.finished
	u.Handle(call)
	if !u.idle || u.gapSeq != seq || u.gapOpen || u.finished != finished || u.inflight != 0 {
		t.Errorf("idle %v, gap %d (was %d) open %v, finished %d, inflight %d; want the report to change nothing", u.idle, u.gapSeq, seq, u.gapOpen, u.finished, u.inflight)
	}
}

// No gap runs while any call is under way: two calls in flight leave it closed until both have returned.
func TestNoGapRunsWhileAnyCallIsUnderWay(t *testing.T) {
	u, clk := newTimedRun(true, v1alpha1.ProgressStepsAndActivity)
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "a"})
	u.Handle(agent.ToolCallEvent{CallID: "c2", Name: "b"})
	u.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"output": "x"}})
	if u.gapOpen {
		t.Fatal("the gap opened with one call still under way")
	}
	clk.pass(u, 30*time.Second)
	if len(u.steps) != 2 {
		t.Fatalf("%d steps after a long wait with a call under way; want the two calls and no idle card", len(u.steps))
	}
	u.Handle(agent.ToolResultEvent{CallID: "c2", Response: map[string]any{"output": "x"}})
	if !u.gapOpen {
		t.Error("the gap did not open when the last call returned")
	}
}
