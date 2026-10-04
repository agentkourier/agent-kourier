package live

import (
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// newRun is a Run that is a stream with steps, or the update path, or a stream on a platform with no steps.
func newRun(streaming, steps bool) *Run {
	return &Run{
		streaming: streaming, eligible: streaming && steps, caps: chat.Caps{Streams: streaming, Steps: steps, MaxText: 1000},
		texts: map[a2a.ArtifactID]string{}, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// TestHandleAfterTheEndStoresNothing: events that arrive once the run is over are dropped, not kept.
func TestHandleAfterTheEndStoresNothing(t *testing.T) {
	u := newRun(true, true)
	close(u.done)
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "late"})
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	if len(u.texts) != 0 || len(u.order) != 0 || len(u.steps) != 0 {
		t.Fatalf("a run that ended kept %v and %d steps", u.texts, len(u.steps))
	}
}

// TestAToolCallIsContentWhereItIsShown: with only a tool call to show, the update path has a message (its status
// line), a stream that has steps has the step, and a stream whose platform has none has nothing, and must not start
// on an empty text.
func TestAToolCallIsContentWhereItIsShown(t *testing.T) {
	for _, tc := range []struct {
		name             string
		streaming, steps bool
		want             bool
	}{
		{"the update path", false, true, true},
		{"a stream with steps", true, true, true},
		{"a stream on a platform with no steps", true, false, false},
	} {
		u := newRun(tc.streaming, tc.steps)
		u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
		if v := u.snapshot(); v.ok != tc.want {
			t.Errorf("%s: a lone tool call gives content: %v; want %v", tc.name, v.ok, tc.want)
		}
	}
}

// A call that is reported again is the same step, and a result that names no step, or a step with its result already,
// changes nothing.
func TestAStepIsBegunOnceAndFinishedOnce(t *testing.T) {
	u := newRun(true, true)
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	u.Handle(agent.ToolResultEvent{CallID: "nobody", Response: map[string]any{"output": "x"}})
	if v := u.snapshot(); len(v.steps) != 1 || v.steps[0].State != chat.StepRunning {
		t.Fatalf("steps %+v; want one, still running", v.steps)
	}
	u.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"output": "x"}})
	u.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"error": "late"}})
	if v := u.snapshot(); len(v.steps) != 1 || v.steps[0].State != chat.StepDone {
		t.Fatalf("steps %+v; want one, done, which a second result does not change", v.steps)
	}
}

// Calls with no ID cannot be told apart, so each is a step of its own, and none is finished by a result.
func TestCallsWithNoIDAreStepsOfTheirOwn(t *testing.T) {
	u := newRun(true, true)
	u.Handle(agent.ToolCallEvent{Name: "a"})
	u.Handle(agent.ToolCallEvent{Name: "b"})
	u.Handle(agent.ToolResultEvent{Response: map[string]any{"output": "x"}})
	if v := u.snapshot(); len(v.steps) != 2 || v.steps[0].State != chat.StepRunning || v.steps[1].State != chat.StepRunning {
		t.Fatalf("steps %+v; want two, both still running", v.steps)
	}
}

func TestATurnShowsAtMostMaxStepsSteps(t *testing.T) {
	u := newRun(true, true)
	for i := range maxSteps + 20 {
		u.Handle(agent.ToolCallEvent{CallID: fmt.Sprint("c", i), Name: "search"})
	}
	if v := u.snapshot(); len(v.steps) != maxSteps {
		t.Fatalf("%d steps, want %d", len(v.steps), maxSteps)
	}
}

// A turn that is not a stream, or whose platform has no steps, keeps none: nothing is held for a feature that is not used.
func TestNoStepIsKeptWhereStepsAreNotShown(t *testing.T) {
	for _, u := range []*Run{newRun(false, true), newRun(true, false)} {
		u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
		if len(u.steps) != 0 {
			t.Errorf("a run with streaming %v and steps %v kept %d steps", u.streaming, u.caps.Steps, len(u.steps))
		}
	}
}

// A step with no result when the turn is final is ended, and the line of the update path is gone.
func TestAStepWithNoResultIsEndedOnceTheTurnIsFinal(t *testing.T) {
	u := newRun(true, true)
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	u.Handle(agent.ToolCallEvent{CallID: "c2", Name: "fetch"})
	u.Handle(agent.ToolResultEvent{CallID: "c2", Response: map[string]any{"error": "boom"}})
	u.Handle(agent.FinalEvent{})
	v := u.snapshot()
	if len(v.steps) != 2 || v.steps[0].State != chat.StepEnded || v.steps[1].State != chat.StepFailed {
		t.Fatalf("steps %+v; want the first ended and the second failed", v.steps)
	}
}

// The step follows the text of the artifacts that were there when its call was made, and no more.
func TestAStepFollowsTheTextThatCameBeforeItsCall(t *testing.T) {
	u := newRun(true, true)
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "Let me look."})
	u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	u.Handle(agent.TextEvent{ArtifactID: "b", Text: "Found it."})
	u.Handle(agent.ToolCallEvent{CallID: "c2", Name: "fetch"})
	v := u.snapshot()
	if v.body != "Let me look.\n\nFound it." || len(v.pos) != 2 || v.body[:v.pos[0]] != "Let me look." || v.pos[1] != len(v.body) {
		t.Fatalf("body %q, positions %v; want the first step after the first text and the second after all of it", v.body, v.pos)
	}
}
