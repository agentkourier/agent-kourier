package live_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/render/live"
)

// parked says whether a Run goroutine is waiting for an event: blocked in the select at the top of its loop, which
// is a goroutine in "select" whose innermost frame is run itself, and not one asleep in a throttle window (sleep
// is the innermost frame), in a write, or between them. It reads the goroutines' stacks, as runsAlive does, since
// the renderer says nothing when it decides to write nothing, and that is what is to be proved: that it has looked
// at the state and chosen not to.
func parked() bool {
	buf := make([]byte, 1<<20)
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		lines := strings.Split(g, "\n")
		if len(lines) > 1 && strings.Contains(lines[0], "[select") && strings.Contains(lines[1], "live.(*Run).run(") {
			return true
		}
	}
	return false
}

// TestAnEventThatChangesNothingShownWritesNothing: the window opens, the renderer looks at the state, finds that it
// would show what is shown already, and writes nothing, since an edit that changes nothing still counts against
// Slack's rate limit. Without that, every event that is no change (a text event repeated, the result of a call
// that was never started, a tool call in a stream, which has no tool line) would cost an edit a window.
func TestAnEventThatChangesNothingShownWritesNothing(t *testing.T) {
	same := func(run *live.Run) { run.Handle(text("a", "Hel")) }
	unknownResult := func(run *live.Run) { run.Handle(agent.ToolResultEvent{CallID: "never-started", Name: "x"}) }
	toolInAStream := func(run *live.Run) { run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"}) }
	for _, tc := range []struct {
		name      string
		recipient string // a stream if there is one
		event     func(*live.Run)
		first     string // the request that makes the first message
		edit      string // the request that would edit it
	}{
		{"a text event repeated", "", same, "/chat.postMessage", "/chat.update"},
		{"the result of a call never started", "", unknownResult, "/chat.postMessage", "/chat.update"},
		{"a text event repeated, in a stream", "U1", same, "/chat.startStream", "/chat.appendStream"},
		{"the result of a call never started, in a stream", "U1", unknownResult, "/chat.startStream", "/chat.appendStream"},
		{"a tool call in a stream, which has no tool line", "U1", toolInAStream, "/chat.startStream", "/chat.appendStream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: tc.recipient})
			run.Handle(text("a", "Hel"))
			eventually(t, "the first message", func() bool { return len(e.rec.sent(tc.first)) == 1 })
			tc.event(run)
			eventually(t, "the renderer to wait for its window", func() bool { return e.clk.waiting() == 1 })
			e.clk.advance(throttle)
			// The window is open. The renderer has seen it when it is waiting for an event again, and what it
			// wrote, if it wrote anything, is on the wire by then.
			eventually(t, "the renderer to look at the state and wait again", func() bool { return e.clk.waiting() == 0 && parked() })
			for _, path := range []string{tc.edit, "/chat.update"} {
				if n := len(e.rec.sent(path)); n != 0 {
					t.Errorf("%d %s requests; want none for an event that shows nothing new", n, path)
				}
			}
			run.Handle(text("a", "Hello")) // and the renderer is not stuck: a change is written
			eventually(t, "the change to be written", func() bool {
				if e.clk.waiting() > 0 {
					e.clk.advance(throttle)
				}
				return len(e.rec.sent(tc.edit)) == 1
			})
		})
	}
}
