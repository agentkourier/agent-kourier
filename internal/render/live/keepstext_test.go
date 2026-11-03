package live_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render/live"
)

// earlierText is what each test leaves in the thread for a recovered run to find: a message of the word "partial" and nothing
// else, a stream that an earlier process left open, or a post of a turn that was never a stream.
var earlierText = []struct {
	name string
	left func(e *env, t *testing.T)
}{
	{"a stream", func(e *env, t *testing.T) { e.leftOpen(t, "partial") }},
	{"a post", func(e *env, t *testing.T) { e.postedByUpdate(t) }},
}

// assertNoRetries fails if the run took the refusal for a write that failed: Slack answers block_mismatch every time. It reads
// the logs, which the run writes, so the run must have ended.
func (e *env) assertNoRetries(t *testing.T) {
	t.Helper()
	if l := e.logs.String(); strings.Contains(l, "writing the message failed") || strings.Contains(l, "giving up") {
		t.Errorf("the refusal was retried as a failed write:\n%s", l)
	}
}

// A recovered run with no text of its own cannot put a Working… card or a step in the place of a message that holds only
// text: Slack refuses that with block_mismatch, whatever the try (measured on 2026-11-03). The run takes the first answer,
// leaves the text as it is and tries no more, however many windows pass and steps come. Text then goes through, cards
// and all. Retrying was what logged the error every 2 seconds.
func TestARecoveredRunLeavesATextItCannotReplaceUntilItHasTextOfItsOwn(t *testing.T) {
	for _, tc := range earlierText {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.left(e, t)
			again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: "U1", Recovered: true})
			e.until(t, "the update of the Working… card", 8*time.Second, func() bool { return len(e.rec.sent("/chat.update")) == 1 })
			again.Handle(call("c1", "search", map[string]any{"q": "up"}))
			again.Handle(result("c1", map[string]any{"output": "x"}))
			e.elapse(6 * throttle)
			if m := e.reply(); m.Text != "partial" || m.Streaming {
				t.Fatalf("message %+v; want the text that was there, not streaming", m)
			}
			if n := len(e.rec.sent("/chat.update")); n != 1 {
				t.Errorf("%d updates; want the one that was refused", n)
			}

			again.Handle(text("a", "the whole answer"))
			again.Handle(agent.FinalEvent{})
			eventually(t, "the answer", func() bool { e.clk.advance(throttle); return e.reply().Text == "the whole answer" })
			e.finish(t, again)
			m := e.reply()
			if got := m.Tasks(); len(got) == 0 || got[0].Title != "search" {
				t.Errorf("tasks %+v; want the step, now that text can go with it", got)
			}
			if !strings.Contains(string(m.Blocks), "the whole answer") {
				t.Errorf("blocks %s; want the answer", m.Blocks)
			}
			e.assertNoRetries(t)
		})
	}
}

// A run that ends without text of its own leaves the text as it found it: a final with cards, which cannot go in its place, one
// with nothing to show, and one that is cut short. "(no output)" would say less than the message says already.
func TestARecoveredRunThatEndsWithNoTextLeavesTheTextItFound(t *testing.T) {
	for _, tc := range earlierText {
		for _, end := range []struct {
			name string
			end  func(run *live.Run, cancel func())
		}{
			{"a final with its cards", func(run *live.Run, _ func()) {
				run.Handle(call("c1", "search", nil))
				run.Handle(agent.FinalEvent{})
			}},
			{"a final with nothing", func(run *live.Run, _ func()) { run.Handle(agent.FinalEvent{}) }},
			{"cut short", func(_ *live.Run, cancel func()) { cancel() }},
		} {
			t.Run(tc.name+", "+end.name, func(t *testing.T) {
				e := setup(t)
				tc.left(e, t)
				again, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: "U1", Recovered: true, Footer: []chat.Part{chat.Context{Text: "rate me"}}})
				e.until(t, "the update of the Working… card", 8*time.Second, func() bool { return len(e.rec.sent("/chat.update")) == 1 })
				end.end(again, cancel)
				eventually(t, "the run to end", func() bool {
					e.clk.advance(throttle)
					select {
					case <-again.Done():
						return true
					default:
						return false
					}
				})
				if m := e.reply(); m.Text != "partial" || m.Streaming || strings.Contains(string(m.Blocks), "(no output)") || strings.Contains(string(m.Blocks), "rate me") {
					t.Errorf("message %+v, blocks %s; want the text it held, as it held it", m, m.Blocks)
				}
				if n := len(e.rec.sent("/chat.update")); n != 1 {
					t.Errorf("%d updates; want the one that was refused", n)
				}
				e.assertNoRetries(t)
			})
		}
	}
}

// The sequence of an alert thread, as a released broker's messages and logs show it on 2026-11-03: an attempt shows Working…,
// its send fails and the run is cut, which leaves "(no output)", text and nothing else; the agent is still not ready, so every
// attempt after it shows Working… on that message after the delay. Each attempt makes the one refused call, and none retries
// it. The answer then replaces the placeholder.
func TestAttemptsOnThePlaceholderOfAnEarlierOneDoNotRetryTheRefusal(t *testing.T) {
	e := setup(t)
	turn := live.Turn{Mode: v1alpha1.OutputLive, Progress: sa}
	first, cancel := e.startFor(t, turn)
	e.until(t, "the Working…", 6*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	cancel()
	e.finish(t, first)
	if m := e.reply(); !strings.Contains(string(m.Blocks), "(no output)") || strings.Contains(string(m.Blocks), "Working") {
		t.Fatalf("blocks %s; want the placeholder of a turn cut short", m.Blocks)
	}

	turn.Recovered = true
	updates := func() int { return len(e.rec.sent("/chat.update")) }
	for attempt := range 3 {
		before := updates()
		run, end := e.startFor(t, turn)
		e.until(t, "the refused update", 8*time.Second, func() bool { return updates() > before })
		e.elapse(6 * throttle)
		if got := updates(); got != before+1 {
			t.Errorf("attempt %d: %d updates; want the one that was refused", attempt, got-before)
		}
		end()
		e.finish(t, run)
	}
	if m := e.reply(); !strings.Contains(string(m.Blocks), "(no output)") {
		t.Errorf("blocks %s; want the placeholder still there", m.Blocks)
	}
	e.assertNoRetries(t)

	last, _ := e.startFor(t, turn)
	last.Handle(text("a", "the whole answer"))
	last.Handle(agent.FinalEvent{})
	e.finish(t, last)
	if m := e.reply(); m.Text != "the whole answer" || strings.Contains(string(m.Blocks), "(no output)") {
		t.Errorf("message %+v, blocks %s; want the answer in place of the placeholder", m, m.Blocks)
	}
}
