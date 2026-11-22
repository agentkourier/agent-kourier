package live_test

import (
	"fmt"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// approvalPause is the final event of a task paused for the approval of the calls named.
func approvalPause(calls ...string) agent.FinalEvent {
	return agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, AwaitingApproval: calls}
}

// Cards says, at the pause, which calls the pause waits on the turn's message shows as a card with the summary of their
// arguments: what the ending of the approval may then leave out. It is the run's own record of its cards, not the Binding's
// modes: a turn that shows no cards (activity, final, a platform without steps, steps the platform refused, a stream that
// had to be posted instead) and a call whose card was dropped name none, and a question names none.
func TestCardsAreTheAwaitedCallsTheMessageShowsWithTheirArguments(t *testing.T) {
	shown := map[string]string{"c2": "to=6"}
	for _, tc := range []struct {
		name  string
		turn  live.Turn
		prep  func(t *testing.T, e *env) // makes the platform, before the run starts
		early func(t *testing.T, e *env, run *live.Run)
		pause agent.FinalEvent
		want  map[string]string
	}{
		{name: "a stream in steps", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"}, pause: approvalPause("c2"), want: shown},
		{name: "a stream in steps-and-activity", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Progress: v1alpha1.ProgressStepsAndActivity}, pause: approvalPause("c2"), want: shown},
		{name: "an alert thread in steps", turn: live.Turn{Mode: v1alpha1.OutputLive}, pause: approvalPause("c2"), want: shown},
		{name: "a stream in activity", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Progress: v1alpha1.ProgressActivity}, pause: approvalPause("c2")},
		{name: "an alert thread in activity", turn: live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressActivity}, pause: approvalPause("c2")},
		{name: "output final", turn: live.Turn{Mode: v1alpha1.OutputFinal, Recipient: "U1"}, pause: approvalPause("c2")},
		{name: "a platform without steps", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"},
			prep: func(t *testing.T, e *env) { e.spy(t, false, nil) }, pause: approvalPause("c2")},
		{name: "a question", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"}, pause: approvalPause()},
		{name: "a call that made no card", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"}, pause: approvalPause("c9")},
		{name: "steps the platform refused", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"},
			prep: func(t *testing.T, e *env) { e.spy(t, true, func(int) error { return errRefused }) },
			early: func(t *testing.T, e *env, run *live.Run) { // the text after the refused step is streamed once the steps are off
				run.Handle(call("c1", "search", nil))
				run.Handle(text("a", "Looking."))
				e.until(t, "the text after the refused step", 6*time.Second, func() bool { return e.reply().Text == "Looking." })
			},
			pause: approvalPause("c2")},
		{name: "cards an alert thread's update had refused", turn: live.Turn{Mode: v1alpha1.OutputLive},
			prep: func(t *testing.T, e *env) {
				e.use(refusesSteps{Platform: e.platform, refused: new(atomic.Int32)})
				e.live = newRenderer(t, e.cfg)
			},
			early: func(t *testing.T, e *env, run *live.Run) {
				run.Handle(call("c1", "search", nil))
				testutil.Eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
				run.Handle(result("c1", map[string]any{"items": 1}))
				e.until(t, "the line, sent again without the refused cards", 6*time.Second, func() bool { return e.reply().Text == "Ran search()" })
			},
			pause: approvalPause("c2")},
		{name: "a stream the platform refused, posted instead", turn: live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"},
			prep: func(t *testing.T, e *env) {
				e.use(spy{Platform: e.platform, caps: e.platform.Capabilities(), steps: new(atomic.Int32), streamFail: fmt.Errorf("start: %w", errRefused)})
				e.live = newRenderer(t, e.cfg)
			},
			early: func(t *testing.T, e *env, run *live.Run) {
				run.Handle(text("a", "Looking."))
				e.until(t, "the post", 6*time.Second, func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
			},
			pause: approvalPause("c2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			if tc.prep != nil {
				tc.prep(t, e)
			}
			run, _ := e.startFor(t, tc.turn)
			if tc.early != nil {
				tc.early(t, e, run)
			}
			run.Handle(call("c2", "rollback", map[string]any{"to": 6}))
			run.Handle(result("c2", map[string]any{"error": "the call requires confirmation"}))
			run.Handle(tc.pause)
			if got := run.Cards(); !maps.Equal(got, tc.want) { // as the tap asks: at once, on the stream's goroutine
				t.Errorf("Cards at the pause = %v; want %v", got, tc.want)
			}
			e.finish(t, run)
			if got := run.Cards(); !maps.Equal(got, tc.want) {
				t.Errorf("Cards once the run is over = %v; want %v", got, tc.want)
			}
		})
	}
}

// A listed message shows the latest cards and drops the rest: a call whose card was dropped is no card of the message.
func TestCardsLeaveOutACallWhoseCardADroppedListNoLongerShows(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
	run.Handle(call("c0", "delete_pod", map[string]any{"name": "web-0"}))
	for i := 1; i <= 20; i++ {
		run.Handle(call(fmt.Sprintf("c%d", i), "get", nil))
	}
	run.Handle(call("c21", "delete_pod", map[string]any{"name": "web-1"}))
	run.Handle(approvalPause("c0", "c21"))
	want := map[string]string{"c21": `name="web-1"`}
	if got := run.Cards(); !maps.Equal(got, want) {
		t.Errorf("Cards = %v; want %v: the first call's card was dropped from the list", got, want)
	}
	e.finish(t, run)
}

// The sink gives the tap the cards of the turn whose handler it gave: those of the run that renders it.
func TestTheSinkGivesTheCardsOfTheTurnItRenders(t *testing.T) {
	e := setup(t)
	key := session.Key{Connection: "ns/conn", Channel: "C1", Thread: e.root}
	h, cards := e.sink(t, e.ob).TurnCards(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: "U1", Output: v1alpha1.OutputLive})
	h(call("c2", "rollback", map[string]any{"to": 6}))
	h(approvalPause("c2"))
	if got, want := cards(), map[string]string{"c2": "to=6"}; !maps.Equal(got, want) {
		t.Errorf("cards = %v; want %v", got, want)
	}
	e.drain(t, e.live)
}
