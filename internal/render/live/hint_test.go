package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// In a Binding with chat.threadReplies: mention, an unmentioned reply is ignored, so the final message of each turn says how to
// ask a follow-up (D38): a fixed context line in the footer, on exactly the messages the feedback buttons are on, and above them.

const hint = "Mention me in this thread to ask a follow-up."

// ended is a task that stopped for input.
func ended(s a2a.TaskState) agent.FinalEvent {
	return agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: s}}}
}

// hintScenario is a turn's path to its end, and whether the end is a message the footer belongs on.
type hintScenario struct {
	name   string
	user   string // the turn's author: a person's turn streams in a live Binding, an alert's is posted
	output v1alpha1.OutputMode
	script func(t *testing.T, e *env, h agent.Handler)
	// settled says the turn's last write is in the thread and no more are to come.
	settled func(e *env) bool
	footer  bool // the final message ends with the footer
}

func shown(text string) func(e *env) bool {
	return func(e *env) bool { return e.reply().Text == text && !e.reply().Streaming }
}

// hintScenarios are the paths a final message takes to the thread: a post updated to its final text, a post made once, a
// stream stopped with the footer, a stream that diverged and was replaced, and the ends that carry no footer.
func hintScenarios() []hintScenario {
	answer := func(_ *testing.T, _ *env, h agent.Handler) {
		h(text("a", "the answer"))
		h(agent.FinalEvent{})
	}
	return []hintScenario{
		{"posted", session.SystemUser, v1alpha1.OutputLive, func(t *testing.T, e *env, h agent.Handler) {
			h(text("a", "partial"))
			eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
			answer(t, e, h)
		}, shown("the answer"), true},
		{"final mode", "U1", v1alpha1.OutputFinal, answer, shown("the answer"), true},
		{"streamed", "U1", v1alpha1.OutputLive, func(t *testing.T, e *env, h agent.Handler) {
			h(text("a", "the"))
			eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
			answer(t, e, h)
		}, shown("the answer"), true},
		{"a diverged stream", "U1", v1alpha1.OutputLive, func(t *testing.T, e *env, h agent.Handler) {
			h(text("a", "Hello there"))
			eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
			h(text("a", "Goodbye")) // not an extension of what was streamed
			h(text("a", "Goodbye, all"))
			h(agent.FinalEvent{})
		}, shown("Goodbye, all"), true},
		{"input required, posted", session.SystemUser, v1alpha1.OutputLive, func(_ *testing.T, _ *env, h agent.Handler) {
			h(text("a", "Which database?"))
			h(ended(a2a.TaskStateInputRequired))
		}, shown("Which database?"), false},
		{"input required, streamed", "U1", v1alpha1.OutputLive, func(_ *testing.T, _ *env, h agent.Handler) {
			h(text("a", "Which database?"))
			h(ended(a2a.TaskStateInputRequired))
		}, shown("Which database?"), false},
		{"auth required, final mode", "U1", v1alpha1.OutputFinal, func(_ *testing.T, _ *env, h agent.Handler) {
			h(text("a", "Sign in to continue."))
			h(ended(a2a.TaskStateAuthRequired))
		}, shown("Sign in to continue."), false},
		{"no output, posted", session.SystemUser, v1alpha1.OutputLive, func(t *testing.T, e *env, h agent.Handler) {
			h(agent.ToolCallEvent{CallID: "c1", Name: "search"})
			eventually(t, "the tool line to be posted", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
			h(agent.FinalEvent{})
		}, func(e *env) bool { return strings.Contains(e.reply().Text, "(no output)") }, false},
		{"only steps, streamed", "U1", v1alpha1.OutputLive, func(_ *testing.T, _ *env, h agent.Handler) {
			h(agent.ToolCallEvent{CallID: "c1", Name: "search"})
			h(agent.ToolResultEvent{CallID: "c1"})
			h(agent.FinalEvent{})
		}, func(e *env) bool { return len(e.rec.sent("/chat.stopStream")) == 1 }, false},
	}
}

// TestTheFinalMessageEndsWithTheHintOnlyInAMentionBinding crosses every path a final message takes with the Binding's mode and
// with its feedback: the hint is there once if the mode is mention and the end carries the footer, the buttons if the feedback is
// on and the end carries the footer, the hint above the question and the buttons, and nothing of either on an end that does not.
func TestTheFinalMessageEndsWithTheHintOnlyInAMentionBinding(t *testing.T) {
	for _, sc := range hintScenarios() {
		for _, mode := range []v1alpha1.ThreadReplies{v1alpha1.ThreadRepliesMention, v1alpha1.ThreadRepliesAll, ""} {
			for _, feedback := range []store.FeedbackKind{store.FeedbackAnswer, ""} {
				name := sc.name + "/" + string(mode) + "/feedback=" + string(feedback)
				t.Run(name, func(t *testing.T) {
					e := setup(t)
					key := thread
					key.Thread = e.root
					h := e.sink(t, e.ob).Turn(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: sc.user, Output: sc.output, Feedback: feedback, ThreadReplies: mode})
					sc.script(t, e, h)
					eventually(t, "the turn to be shown", func() bool { e.clk.advance(throttle); return sc.settled(e) })
					e.drain(t, e.live)
					blocks := string(e.reply().Blocks)
					wantHint := sc.footer && mode == v1alpha1.ThreadRepliesMention
					wantButtons := sc.footer && feedback != ""
					if got := strings.Count(blocks, hint); got != btoi(wantHint) {
						t.Errorf("the hint is in the message %d times, want %d: %s", got, btoi(wantHint), blocks)
					}
					if got := strings.Count(blocks, "Was this useful?"); got != btoi(wantButtons) {
						t.Errorf("the question is in the message %d times, want %d: %s", got, btoi(wantButtons), blocks)
					}
					if wantHint && wantButtons && strings.Index(blocks, hint) > strings.Index(blocks, "Was this useful?") {
						t.Errorf("the hint follows the question; want it above, so that the buttons end the message: %s", blocks)
					}
					if stops := e.rec.sent("/chat.stopStream"); sc.name == "a diverged stream" && len(stops) == 1 && stops[0].Has("blocks") {
						t.Errorf("a stream that diverged was stopped with blocks %s; the update carries the footer", stops[0].Get("blocks"))
					}
				})
			}
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A stream is stopped with the footer, hint and buttons both, once, and the text is not sent again.
func TestAStreamIsStoppedWithTheHintAndTheButtons(t *testing.T) {
	e := setup(t)
	key := thread
	key.Thread = e.root
	h := e.sink(t, e.ob).Turn(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: "U1", Output: v1alpha1.OutputLive, Feedback: store.FeedbackAnswer, ThreadReplies: v1alpha1.ThreadRepliesMention})
	h(text("a", "Hello"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	h(text("a", "Hello world"))
	h(agent.FinalEvent{})
	eventually(t, "the stream to stop", func() bool { e.clk.advance(throttle); return shown("Hello world")(e) })
	e.drain(t, e.live)
	stops := e.rec.sent("/chat.stopStream")
	if b := stops[0].Get("blocks"); strings.Count(b, hint) != 1 || !strings.Contains(b, "feedback-useful") {
		t.Fatalf("the stop carries blocks %s; want the hint once and the buttons", b)
	}
	if m := e.reply(); m.Streaming || strings.Count(string(m.Blocks), hint) != 1 || strings.Count(string(m.Blocks), "Hello world") != 1 || e.updates() != 0 {
		t.Fatalf("after the stop: streaming %v, blocks %s, %d updates; want the text once, the hint once, and no update", m.Streaming, m.Blocks, e.updates())
	}
}

// crashOnce is a Sender whose first post is written to the outbox and never made, as when the process dies between the two: it
// keeps the row's payload as it was stored. Every later post is the outbox's own, which finds the row and posts from it.
type crashOnce struct {
	ob     *outbox.Outbox
	st     store.Outbox
	once   sync.Once
	stored chan string
}

func (c *crashOnce) Send(ctx context.Context, p outbox.Post) (chat.Ref, error) {
	var crash error
	c.once.Do(func() {
		id, err := c.ob.Enqueue(ctx, p)
		if err != nil {
			crash = err
			return
		}
		row, err := c.st.GetOutbox(ctx, id)
		c.stored <- row.Payload
		if crash = err; crash == nil {
			crash = errors.New("the process died")
		}
	})
	if crash != nil {
		return chat.Ref{}, crash
	}
	return c.ob.Send(ctx, p)
}

// The footer is in the stored form of the message: the outbox row of a post holds the hint above the question and the buttons, and
// the post made from that row alone, as after a crash, ends the same way.
func TestTheHintSurvivesTheOutboxRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		feedback store.FeedbackKind
		want     []string // the kinds of the parts after the text, in order
	}{
		{"with the buttons", store.FeedbackAnswer, []string{"hint", "question", "buttons"}},
		{"alone", "", []string{"hint"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			sender := &crashOnce{ob: e.ob, st: e.store, stored: make(chan string, 1)}
			cfg := e.cfg
			cfg.Sender = sender
			key := thread
			key.Thread = e.root
			renderer := newRenderer(t, cfg)
			h := live.NewSink(renderer, e.ob, slog.New(slog.NewTextHandler(io.Discard, nil))).Turn(t.Context(),
				session.TurnRef{Key: key, MessageID: "turn-1", UserID: "U1", Output: v1alpha1.OutputFinal, Feedback: tc.feedback, ThreadReplies: v1alpha1.ThreadRepliesMention})
			h(text("a", "the answer"))
			h(agent.FinalEvent{})
			var payload string
			select {
			case payload = <-sender.stored:
			case <-timeoutAfter():
				t.Fatal("the post was never written")
			}
			var out chat.Outgoing
			if err := json.Unmarshal([]byte(payload), &out); err != nil || out.Message == nil {
				t.Fatalf("payload %s: %v", payload, err)
			}
			var got []string
			for _, p := range out.Message.Parts[1:] { // after the body
				switch p := p.(type) {
				case chat.Context:
					got = append(got, map[string]string{hint: "hint", "Was this useful?": "question"}[p.Text])
				case chat.Buttons:
					got = append(got, "buttons")
				default:
					got = append(got, fmt.Sprintf("%T", p))
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the stored parts after the text are %v, in %s; want %v", got, payload, tc.want)
			}
			eventually(t, "the post to be made from the row", func() bool { e.clk.advance(throttle); return e.reply().Text == "the answer" })
			e.drain(t, renderer)
			blocks := string(e.reply().Blocks)
			if strings.Count(blocks, hint) != 1 || strings.Contains(blocks, "Was this useful?") != (tc.feedback != "") {
				t.Fatalf("the posted message has blocks %s; want what the row held", blocks)
			}
		})
	}
}

// FollowUpHint is the one place the line's wording is.
func TestFollowUpHintIsTheFixedLine(t *testing.T) {
	if live.FollowUpHint != hint {
		t.Fatalf("FollowUpHint = %q; want %q", live.FollowUpHint, hint)
	}
}
