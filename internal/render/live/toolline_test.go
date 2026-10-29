package live_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
)

// The tests below pin the status line of the update path (an alert thread) on a platform without steps, where every write
// shows it: it names the latest tool call in the tense of how far the call has got, and a call that has returned replaces
// the line that said it was under way. On a platform with steps only the first post shows it (listed_test.go).

// said is the blocks of the thread's last message, which carry the status line.
func (e *env) said() string { return string(e.reply().Blocks) }

// requests is the blocks of every message sent so far, the posts first and then the updates, each in the order sent.
func (e *env) requests() []string {
	var out []string
	for _, path := range []string{"/chat.postMessage", "/chat.update"} {
		for _, f := range e.rec.sent(path) {
			out = append(out, f.Get("blocks"))
		}
	}
	return out
}

func TestACallThatReturnsBeforeTheFirstWriteIsPostedAsRan(t *testing.T) {
	e := setup(t)
	// The status line is the update path's on a platform without steps; on one with steps the updates show cards (listed_test.go).
	e.spy(t, false, nil)
	release := e.hold() // the first write is not throttled: it must not be made before both events are in
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search", Args: map[string]any{"q": "pods"}})
	run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "search"})
	release()
	eventually(t, "the post", func() bool { return strings.Contains(e.said(), "search") })
	if got := e.said(); !strings.Contains(got, `Ran search(q=`) || strings.Contains(got, "Calling") {
		t.Fatalf("blocks %s; want the line in past tense, a call that has returned being no call under way", got)
	}
	if m := e.reply(); !strings.HasPrefix(m.Text, "Ran search(") {
		t.Errorf("fallback text %q; want the line as plain text", m.Text)
	}
}

func TestAReturnedCallReplacesTheLineThatSaidItWasUnderWay(t *testing.T) {
	for name, tc := range map[string]struct {
		response any
		want     string
	}{
		"ran":                   {map[string]any{"items": 3}, "Ran search(q="},
		"failed on an error":    {map[string]any{"error": "no such pod"}, `) failed`},
		"failed on a tool flag": {map[string]any{"isError": true}, `) failed`},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.spy(t, false, nil) // the status line is the update path's on a platform without steps (listed_test.go)
			run, _ := e.start(t, v1alpha1.OutputLive, false)
			run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search", Args: map[string]any{"q": "pods"}})
			eventually(t, "the post of the line", func() bool {
				return len(e.rec.sent("/chat.postMessage")) == 1 && strings.Contains(e.said(), "Calling search")
			})
			posted := len(e.requests())
			run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "search", Response: tc.response})
			eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
			e.clk.advance(throttle)
			eventually(t, "the line after the result", func() bool { return strings.Contains(e.said(), tc.want) })
			if got := e.said(); strings.Contains(got, "Calling") || (tc.want != "Ran search(q=" && strings.Contains(got, "Ran ")) {
				t.Errorf("blocks %s; want the line of the outcome and no word of a call under way", got)
			}
			for _, blocks := range e.requests()[posted:] {
				if strings.Contains(blocks, "Calling") {
					t.Errorf("a write after the result shows the call as under way: %s", blocks)
				}
			}
			// Text takes the line's place, as it always did.
			run.Handle(text("a", "Found three pods"))
			eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
			e.clk.advance(throttle)
			eventually(t, "the text", func() bool { return e.reply().Text == "Found three pods" })
			if got := e.said(); strings.Contains(got, "search") {
				t.Errorf("blocks %s; want the text to have replaced the line", got)
			}
		})
	}
}

func TestACallReportedAgainAfterItsResultStaysReturned(t *testing.T) {
	e := setup(t)
	// The status line is the update path's on a platform without steps; on one with steps the updates show cards (listed_test.go).
	e.spy(t, false, nil)
	release := e.hold()
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	call := agent.ToolCallEvent{CallID: "c1", Name: "search"}
	run.Handle(call)
	run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "search"})
	run.Handle(call) // the same call, reported again
	release()
	eventually(t, "the post", func() bool { return strings.Contains(e.said(), "search") })
	if got := e.said(); !strings.Contains(got, "Ran search") || strings.Contains(got, "Calling") {
		t.Errorf("blocks %s; want the call, reported again, still returned", got)
	}
}

func TestTheLineFollowsTheLatestOfCallsMadeTogether(t *testing.T) {
	e := setup(t)
	// The status line is the update path's on a platform without steps; on one with steps the updates show cards (listed_test.go).
	e.spy(t, false, nil)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	release := e.hold()
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "first"})
	run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "second"})
	run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "first"}) // not the latest: the line is still of the call under way
	release()
	eventually(t, "the post of the latest call, under way", func() bool { return strings.Contains(e.said(), "Calling second") })
	if got := e.said(); strings.Contains(got, "first") {
		t.Fatalf("blocks %s; want the latest call only", got)
	}
	run.Handle(agent.ToolResultEvent{CallID: "c2", Name: "second"})
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the latest call returned", func() bool { return strings.Contains(e.said(), "Ran second") })
}

func TestAReturnedCallsLineIsReplacedByNoOutputWhenTheTurnEndsWithoutText(t *testing.T) {
	e := setup(t)
	// The status line is the update path's on a platform without steps; on one with steps the updates show cards (listed_test.go).
	e.spy(t, false, nil)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	eventually(t, "the post", func() bool { return strings.Contains(e.said(), "Calling search") })
	run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "search"})
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "(no output)" || strings.Contains(string(m.Blocks), "search") {
		t.Errorf("final message %+v; want no output, and no line left standing", m)
	}
}
