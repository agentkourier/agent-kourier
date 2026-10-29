package live_test

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The footer is what a turn's final message ends with under its text (Turn.Footer): the feedback buttons of spec 8.7. It
// is on the message once, when the turn is final, and never before, so that nobody is asked to rate a reply that is still
// being written.
var footer = []chat.Part{
	chat.Context{Text: "Was this useful?"},
	chat.Buttons{Buttons: []chat.Button{{ID: "feedback-useful", Label: "Useful", Value: "fb1|investigation|useful|100.1|turn-1"}, {ID: "feedback-not-useful", Label: "Not useful", Value: "fb1|investigation|not_useful|100.1|turn-1"}}},
}

// hasFooter says whether the blocks of a message hold the footer's question and both buttons, and how many copies of
// the question.
func hasFooter(blocks string) (buttons bool, questions int) {
	return strings.Contains(blocks, `"action_id":"feedback-useful"`) && strings.Contains(blocks, `"action_id":"feedback-not-useful"`) &&
		strings.Contains(blocks, "fb1|investigation|useful|100.1|turn-1"), strings.Count(blocks, "Was this useful?")
}

// liveTurn is a turn of the test's thread that ends with the footer.
func liveTurn(mode v1alpha1.OutputMode, recipient string) live.Turn {
	return live.Turn{Mode: mode, Recipient: recipient, Footer: footer}
}

func TestTheFinalMessageOfAPostedTurnCarriesTheFooterAndNoEarlierOneDoes(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "partial"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	if buttons, _ := hasFooter(string(e.reply().Blocks)); buttons {
		t.Fatalf("a reply that is still being written carries the buttons: %s", e.reply().Blocks)
	}
	run.Handle(text("a", "partial, and the answer"))
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the update", func() bool { return e.updates() == 1 })
	if buttons, _ := hasFooter(string(e.reply().Blocks)); buttons {
		t.Fatalf("an update before the final carries the buttons: %s", e.reply().Blocks)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	buttons, questions := hasFooter(string(e.reply().Blocks))
	if m := e.reply(); !buttons || questions != 1 || m.Text != "partial, and the answer" {
		t.Fatalf("the final message says %q with blocks %s; want its text and the footer once", m.Text, m.Blocks)
	}
}

// The final text may be the very text already shown: the buttons still arrive.
func TestAFinalWithTheTextAlreadyShownStillGetsTheFooter(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "the whole answer"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if buttons, questions := hasFooter(string(e.reply().Blocks)); !buttons || questions != 1 {
		t.Fatalf("the final message has blocks %s; want the footer once", e.reply().Blocks)
	}
}

func TestAFinalModeTurnPostsTheFooterWithItsOnePost(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputFinal, ""))
	run.Handle(text("a", "the answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if posts := len(e.rec.sent("/chat.postMessage")); posts != 1 {
		t.Fatalf("%d posts; want the one reply", posts)
	}
	if buttons, questions := hasFooter(string(e.reply().Blocks)); !buttons || questions != 1 {
		t.Fatalf("the reply has blocks %s; want the footer once", e.reply().Blocks)
	}
}

func TestAStreamedTurnStopsWithTheFooterAndTheTextOnce(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, "U1"))
	run.Handle(text("a", "Hello"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if buttons, questions := hasFooter(string(m.Blocks)); m.Streaming || !buttons || questions != 1 || strings.Count(string(m.Blocks), "Hello world") != 1 {
		t.Fatalf("after the stop: streaming %v, blocks %s; want the text once and the footer once", m.Streaming, m.Blocks)
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || !strings.Contains(stops[0].Get("blocks"), "feedback-useful") {
		t.Fatalf("stops %v; want one that carries the footer's blocks", stops)
	}
	if n := e.updates(); n != 0 {
		t.Fatalf("%d updates for a stream that did not diverge", n)
	}
}

func TestAStreamThatDivergedGetsTheFooterWithTheUpdateThatReplacesIt(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, "U1"))
	run.Handle(text("a", "Hello there"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Goodbye")) // not an extension of what was streamed
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	run.Handle(text("a", "Goodbye, all"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if buttons, questions := hasFooter(string(e.reply().Blocks)); !buttons || questions != 1 {
		t.Fatalf("the message has blocks %s; want the footer once, from the update", e.reply().Blocks)
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || stops[0].Has("blocks") {
		t.Fatalf("stops %v; want one that is bare: the update carries the footer", stops)
	}
}

// A turn that ended with nothing to show has no reply to rate, and a tool line left standing is replaced by "(no output)".
func TestATurnWithNoOutputHasNoFooter(t *testing.T) {
	e := setup(t)
	// The status line is the update path's on a platform without steps; on one with steps the updates show cards (listed_test.go).
	e.spy(t, false, nil)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, ""))
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	eventually(t, "the tool line to be posted", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); strings.Contains(string(m.Blocks), "feedback-useful") || !strings.Contains(m.Text, "(no output)") {
		t.Fatalf("the message says %q with blocks %s; want no output and no buttons", m.Text, m.Blocks)
	}
}

// A turn with no footer is as it was: no blocks beyond its text.
func TestATurnWithNoFooterCarriesNoButtons(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "the answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if strings.Contains(string(e.reply().Blocks), "feedback") {
		t.Fatalf("a turn with no footer has blocks %s", e.reply().Blocks)
	}
}

// A pause is a final event too, and the message before an agent's question is no answer to rate: only the task's end is rated,
// and when the person has answered, the output that follows is a message of its own, named after the answer, which is the one
// that carries the buttons.
func TestAPausedTurnHasNoFooterAndTheContinuationAfterTheAnswerHasIt(t *testing.T) {
	task := func(s a2a.TaskState) agent.FinalEvent {
		return agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: s}}}
	}
	for _, tc := range []struct {
		name      string
		state     a2a.TaskState
		recipient string
		want      bool
	}{
		{"input required, posted", a2a.TaskStateInputRequired, "", false},
		{"auth required, posted", a2a.TaskStateAuthRequired, "", false},
		{"input required, streamed", a2a.TaskStateInputRequired, "U1", false},
		{"auth required, streamed", a2a.TaskStateAuthRequired, "U1", false},
		{"completed, posted", a2a.TaskStateCompleted, "", true},
		{"completed, streamed", a2a.TaskStateCompleted, "U1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, tc.recipient))
			run.Handle(text("a", "Which database?"))
			run.Handle(task(tc.state))
			e.finish(t, run)
			if buttons, _ := hasFooter(string(e.reply().Blocks)); buttons != tc.want {
				t.Fatalf("a turn that ended %s has blocks %s; want the buttons: %v", tc.state, e.reply().Blocks, tc.want)
			}
		})
	}

	// The continuation: the answer's output, under the answer's own turn, is rated by a value that names that turn.
	e := setup(t)
	key := thread
	key.Thread = e.root
	sink := e.sink(t, e.ob)
	paused := sink.Turn(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: session.SystemUser, Feedback: store.FeedbackInvestigation, Output: v1alpha1.OutputLive})
	paused(text("a", "Which database?"))
	paused(task(a2a.TaskStateInputRequired))
	eventually(t, "the question to be posted", func() bool { e.clk.advance(throttle); return e.reply().Text == "Which database?" })
	answered := sink.Turn(t.Context(), session.TurnRef{Key: key, MessageID: "ans-1", UserID: session.SystemUser, Feedback: store.FeedbackInvestigation, Output: v1alpha1.OutputLive})
	answered(text("a", "Use PostgreSQL."))
	answered(task(a2a.TaskStateCompleted))
	eventually(t, "the continuation to be posted", func() bool { e.clk.advance(throttle); return e.reply().Text == "Use PostgreSQL." })
	e.drain(t, e.live)
	th := e.fake.Thread("C1", e.root)
	var rated []string
	for _, m := range th {
		if strings.Contains(string(m.Blocks), "feedback-useful") {
			rated = append(rated, m.Text)
		}
	}
	if len(rated) != 1 || rated[0] != "Use PostgreSQL." || !strings.Contains(string(th[len(th)-1].Blocks), "|ans-1") {
		t.Fatalf("messages with buttons: %q; want only the continuation, rating the answer's turn ans-1", rated)
	}
}

// A stream that showed steps and no text has no reply to rate: its stop carries no buttons (the guard on the text).
func TestAStreamOfOnlyStepsHasNoFooter(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, liveTurn(v1alpha1.OutputLive, "U1"))
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	run.Handle(agent.ToolResultEvent{CallID: "c1"})
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if len(e.rec.sent("/chat.startStream")) == 0 {
		t.Fatal("the turn did not stream; the test would prove nothing")
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || stops[0].Has("blocks") {
		t.Fatalf("stops %v; want one with no blocks", stops)
	}
	if strings.Contains(string(e.reply().Blocks), "feedback") {
		t.Fatalf("blocks %s; want no buttons", e.reply().Blocks)
	}
}
