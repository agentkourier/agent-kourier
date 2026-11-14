package live_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/testutil"
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

// observed starts turn on a context that counts its metrics, and returns what the final outcome metric says once the run has
// ended.
func (e *env) observed(t *testing.T, turn live.Turn) (run *live.Run, cancel context.CancelFunc, outcome func() string) {
	t.Helper()
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
	t.Cleanup(cancel)
	turn.Source, turn.Channel, turn.Thread = "turn-1", "C1", e.root
	return e.live.Start(ctx, turn), cancel, func() string {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == "agentkourier_render_final_total" {
				if len(f.Metric) != 1 || f.Metric[0].Counter.GetValue() != 1 {
					t.Fatalf("final metric = %v; want one outcome, once", f)
				}
				return f.Metric[0].Label[0].GetValue()
			}
		}
		return ""
	}
}

// A recovered run with no text of its own cannot put a card or a Working… line in the place of a message that holds only text:
// Slack refuses that with block_mismatch, whatever the try (measured on 2026-11-03). In every mode the run takes the first
// answer, leaves the text as it is and tries no more, however many windows pass and steps come. Text then goes through, cards
// and all. Retrying was what logged the error every 2 seconds.
func TestARecoveredRunLeavesATextItCannotReplaceUntilItHasTextOfItsOwn(t *testing.T) {
	for _, mode := range []struct {
		name     string
		progress v1alpha1.ProgressMode
		first    func(run *live.Run)
	}{
		{"steps-and-activity, whose delay makes the first write", sa, func(*live.Run) {}},
		{"steps, whose tool call makes it", v1alpha1.ProgressSteps, func(run *live.Run) { run.Handle(call("c0", "list", nil)) }},
		{"activity, whose counted result makes it", activity, func(run *live.Run) {
			run.Handle(call("c0", "list", nil))
			run.Handle(result("c0", map[string]any{"output": "x"}))
		}},
	} {
		for _, tc := range earlierText {
			t.Run(mode.name+", "+tc.name, func(t *testing.T) {
				e := setup(t)
				tc.left(e, t)
				again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: mode.progress, Recipient: "U1", Recovered: true})
				mode.first(again)
				e.until(t, "the refused update", 8*time.Second, func() bool { return e.updates() == 1 })
				again.Handle(call("c1", "search", map[string]any{"q": "up"}))
				again.Handle(result("c1", map[string]any{"output": "x"}))
				e.elapse(6 * throttle)
				if m := e.reply(); m.Text != "partial" || m.Streaming {
					t.Fatalf("message %+v; want the text that was there, not streaming", m)
				}
				if n := e.updates(); n != 1 {
					t.Errorf("%d updates; want the one that was refused", n)
				}

				again.Handle(text("a", "the whole answer"))
				again.Handle(agent.FinalEvent{})
				testutil.Eventually(t, "the answer", func() bool { e.clk.advance(throttle); return e.reply().Text == "the whole answer" })
				e.finish(t, again)
				m := e.reply()
				if len(m.Tasks()) == 0 {
					t.Errorf("blocks %s; want the cards, now that text can go with them", m.Blocks)
				}
				if !strings.Contains(string(m.Blocks), "the whole answer") {
					t.Errorf("blocks %s; want the answer", m.Blocks)
				}
				e.assertNoRetries(t)
			})
		}
	}
}

// A run that ends without text of its own leaves the text as it found it: a final with cards, which cannot go in its place, one
// with nothing to show, and one that is cut short. "(no output)" would say less than the message says already. It makes the one
// refused call whether it met the refusal before the final or on it, and the outcome says that nothing of the turn's was
// written: none was delivered, and a turn with no final is cancelled.
func TestARecoveredRunThatEndsWithNoTextLeavesTheTextItFound(t *testing.T) {
	for _, tc := range earlierText {
		for _, end := range []struct {
			name, outcome string
			progress      v1alpha1.ProgressMode
			refusedFirst  bool // the delay's write is refused before the end, or the end's own write is the first
			end           func(run *live.Run, cancel context.CancelFunc)
		}{
			{"a final with its cards, refused first", "empty_output", sa, true, func(run *live.Run, _ context.CancelFunc) {
				run.Handle(call("c1", "search", nil))
				run.Handle(agent.FinalEvent{})
			}},
			{"a final with nothing, refused first", "empty_output", sa, true, func(run *live.Run, _ context.CancelFunc) { run.Handle(agent.FinalEvent{}) }},
			{"cut short, refused first", "cancelled", sa, true, func(_ *live.Run, cancel context.CancelFunc) { cancel() }},
			{"a final with its cards, refused there", "empty_output", v1alpha1.ProgressSteps, false, func(run *live.Run, _ context.CancelFunc) {
				run.Handle(call("c1", "search", nil))
				run.Handle(agent.FinalEvent{})
			}},
			{"a final with nothing, refused there", "empty_output", v1alpha1.ProgressSteps, false, func(run *live.Run, _ context.CancelFunc) { run.Handle(agent.FinalEvent{}) }},
		} {
			t.Run(tc.name+", "+end.name, func(t *testing.T) {
				e := setup(t)
				tc.left(e, t)
				turn := live.Turn{Mode: v1alpha1.OutputLive, Progress: end.progress, Recipient: "U1", Recovered: true, Footer: []chat.Part{chat.Context{Text: "rate me"}}}
				release := func() {}
				if !end.refusedFirst { // the first write is made with all the events
					release = e.hold()
					t.Cleanup(release)
				}
				again, cancel, outcome := e.observed(t, turn)
				if end.refusedFirst {
					e.until(t, "the refused update", 8*time.Second, func() bool { return e.updates() == 1 })
				}
				end.end(again, cancel)
				release()
				e.finish(t, again)
				if m := e.reply(); m.Text != "partial" || m.Streaming || strings.Contains(string(m.Blocks), "(no output)") || strings.Contains(string(m.Blocks), "rate me") {
					t.Errorf("message %+v, blocks %s; want the text it held, as it held it", m, m.Blocks)
				}
				if n := e.updates(); n != 1 {
					t.Errorf("%d updates; want the one that was refused", n)
				}
				if got := outcome(); got != end.outcome {
					t.Errorf("final outcome %q; want %q", got, end.outcome)
				}
				e.assertNoRetries(t)
			})
		}
	}
}

// refusesAll is a platform that refuses every update as one that would take a message's text away, which Slack has not been seen
// to do for an update that has text.
type refusesAll struct {
	chat.Platform
	refused *atomic.Int32
}

func (p refusesAll) Update(context.Context, chat.Ref, chat.Outgoing) error {
	p.refused.Add(1)
	return fmt.Errorf("refused: %w", chat.ErrWouldDropText)
}

// An update that carries the reply's text and is refused has lost the reply: it is a failed write, tried and given up on in the
// open, and never a reply delivered.
func TestAnUpdateWithTextThatIsRefusedIsAFailedWrite(t *testing.T) {
	e := setup(t)
	e.leftOpen(t, "partial")
	refused := new(atomic.Int32)
	e.use(refusesAll{Platform: e.platform, refused: refused})
	e.live = newRenderer(t, e.cfg)
	again, _, outcome := e.observed(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(text("a", "the whole answer"))
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)
	if got := outcome(); got != "retry_exhausted" {
		t.Errorf("final outcome %q; want retry_exhausted", got)
	}
	if n := refused.Load(); n != 5 {
		t.Errorf("%d updates; want the 5 attempts of a failed write", n)
	}
	if l := e.logs.String(); !strings.Contains(l, "giving up") || strings.Contains(l, "it stands until") {
		t.Errorf("logs:\n%s\nwant a failed write given up on, not a message left standing", l)
	}
}

// The refusal is about the text an earlier attempt left. Once an update with the turn's own text has passed, the message is this
// run's, and a later update with no text is the platform's to judge again: a card under way is not left up for good.
func TestAnUpdateWithTextEndsTheWait(t *testing.T) {
	e := setup(t)
	e.leftOpen(t, "partial")
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(call("c1", "search", nil))
	e.until(t, "the refused update", throttle, func() bool { return e.updates() == 1 })
	again.Handle(text("a", "hello"))
	testutil.Eventually(t, "the text", func() bool { e.clk.advance(throttle); return e.reply().Text == "hello" })
	again.Handle(text("a", "")) // the agent replaces its text with nothing
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)
	if got := e.reply().Tasks(); len(got) != 1 || got[0].Status != "complete" {
		t.Errorf("tasks %+v; want the card ended, not under way", got)
	}
}

// A stream whose text the agent then takes back has nothing to end on, and its placeholder is no text either: the update that
// would replace the stream's text with it is refused as any update with no text is, and the message stands. It is no failed write,
// though the view carries the placeholder as the text a stream's stop would send.
func TestAStreamWhoseTextIsTakenBackKeepsItWithNoFailedWrite(t *testing.T) {
	e := setup(t)
	run, _, outcome := e.observed(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "hello"))
	testutil.Eventually(t, "the text", func() bool { e.clk.advance(throttle); return strings.Contains(e.reply().Text, "hello") })
	run.Handle(text("a", ""))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "hello" || m.Streaming {
		t.Errorf("message %+v; want the text, the stream stopped", m)
	}
	if n := e.updates(); n != 1 {
		t.Errorf("%d updates; want the one that was refused", n)
	}
	if got := outcome(); got != "empty_output" {
		t.Errorf("final outcome %q; want empty_output", got)
	}
	e.assertNoRetries(t)
}

// The placeholder of a turn cut short is no text of the agent's, so it does not stand in the way of the next attempt: the attempt
// shows its cards on it, with no refused call, and its answer replaces them. Here the turn is an alert thread's, which made this
// message the one that repeated the error, attempt after attempt, while the agent was not ready.
func TestALaterAttemptShowsItsCardsOnThePlaceholderOfAnEarlierOne(t *testing.T) {
	e := setup(t)
	turn := live.Turn{Mode: v1alpha1.OutputLive, Progress: sa}
	first, cancel := e.startFor(t, turn)
	e.until(t, "the Working…", 6*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	cancel()
	e.finish(t, first)
	if m := e.reply(); m.Text != "(no output)" || strings.Contains(string(m.Blocks), "markdown") || strings.Contains(string(m.Blocks), "Working") {
		t.Fatalf("message %+v, blocks %s; want the placeholder of a turn cut short, and no text block", m, m.Blocks)
	}

	turn.Recovered = true
	updates := e.updates()
	later, _ := e.startFor(t, turn)
	e.until(t, "the Working… card", 8*time.Second, func() bool { return len(e.reply().Tasks()) == 1 })
	if m := e.reply(); strings.Contains(string(m.Blocks), "(no output)") {
		t.Errorf("blocks %s; want the card in the place of the placeholder", m.Blocks)
	}
	later.Handle(text("a", "the whole answer"))
	later.Handle(agent.FinalEvent{})
	e.finish(t, later)
	if m := e.reply(); m.Text != "the whole answer" {
		t.Errorf("message %+v; want the answer", m)
	}
	if n := e.updates() - updates; n > 3 {
		t.Errorf("%d updates; want the card, its end and the answer at most", n)
	}
	e.assertNoRetries(t)
}

// A stream's text is all a stream can say, so a stream that ends with nothing says the placeholder as text, unlike a post or an
// update: here one started for a step that the platform then refused. A later attempt holds its cards on it, as on any text,
// and its answer replaces it.
func TestAStreamsPlaceholderIsTextToALaterAttempt(t *testing.T) {
	e := setup(t)
	e.spy(t, true, func(int) error { return chat.ErrNoSteps })
	first := e.stream(t)
	first.Handle(call("c0", "search", nil))
	testutil.Eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	first.Handle(agent.FinalEvent{})
	e.finish(t, first)
	if m := e.reply(); m.Text != "(no output)" || m.Streaming {
		t.Fatalf("message %+v; want the placeholder, stopped", m)
	}

	updates := e.updates()
	later, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	later.Handle(call("c1", "search", nil))
	e.until(t, "the refused update", 8*time.Second, func() bool { return e.updates() > updates })
	e.elapse(6 * throttle)
	if m := e.reply(); m.Text != "(no output)" || len(m.Tasks()) != 0 {
		t.Errorf("message %+v; want the placeholder, and no card on it", m)
	}
	if n := e.updates() - updates; n != 1 {
		t.Errorf("%d updates; want the one that was refused", n)
	}
	later.Handle(text("a", "the whole answer"))
	later.Handle(agent.FinalEvent{})
	e.finish(t, later)
	if m := e.reply(); m.Text != "the whole answer" {
		t.Errorf("message %+v; want the answer", m)
	}
	e.assertNoRetries(t)
}

// A placeholder that a release before this one wrote is text to Slack, and text is what the run cannot replace until it has its
// own. Each attempt makes the one refused call and none retries it, and the answer then replaces it.
func TestAttemptsOnTheTextPlaceholderOfAnEarlierReleaseDoNotRetryTheRefusal(t *testing.T) {
	e := setup(t)
	e.leftOpen(t, "(no output)")
	turn := live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: "U1", Recovered: true}
	for attempt := range 3 {
		before := e.updates()
		run, end := e.startFor(t, turn)
		e.until(t, "the refused update", 8*time.Second, func() bool { return e.updates() > before })
		e.elapse(6 * throttle)
		if got := e.updates(); got != before+1 {
			t.Errorf("attempt %d: %d updates; want the one that was refused", attempt, got-before)
		}
		end()
		e.finish(t, run)
	}
	if m := e.reply(); m.Text != "(no output)" {
		t.Errorf("message %+v; want the placeholder still there", m)
	}
	e.assertNoRetries(t)

	last, _ := e.startFor(t, turn)
	last.Handle(text("a", "the whole answer"))
	last.Handle(agent.FinalEvent{})
	e.finish(t, last)
	if m := e.reply(); m.Text != "the whole answer" {
		t.Errorf("message %+v; want the answer in place of the placeholder", m)
	}
}

// What a run does with the refusal goes through the neutral seam alone: a platform with no Slack under it that says
// chat.ErrWouldDropText is treated as Slack is.
func TestTheRefusalIsHandledThroughTheNeutralSeamOfAnyPlatform(t *testing.T) {
	e := setup(t)
	fake := chattest.NewFake()
	p := &refusesTextless{Platform: fake}
	e.use(p)
	e.live = newRenderer(t, e.cfg)
	e.leftOpen(t, "partial")
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(call("c1", "search", nil))
	testutil.Eventually(t, "the refused update", func() bool { return p.refused.Load() == 1 })
	e.elapse(2 * throttle)
	if n := p.refused.Load(); n != 1 {
		t.Errorf("%d refused updates; want the one", n)
	}
	again.Handle(text("a", "the whole answer"))
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)
	got := fake.Messages()
	if len(got) != 1 || got[0].Updates == 0 || !slices.Contains(got[0].Out.Message.Parts, chat.Part(chat.Body{Markdown: "the whole answer"})) {
		t.Errorf("messages %+v; want the answer, in an update after the refusal", got)
	}
	e.assertNoRetries(t)
}

// refusesTextless refuses an update whose message has no body, as a platform that cannot take a message's text away would, and
// passes every other. It refuses by what the write holds, so it cannot land on the wrong one however the run's timers fall.
type refusesTextless struct {
	chat.Platform
	refused atomic.Int32
}

func (p *refusesTextless) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	if o.Message != nil && !slices.ContainsFunc(o.Message.Parts, func(pt chat.Part) bool { _, ok := pt.(chat.Body); return ok }) {
		p.refused.Add(1)
		return fmt.Errorf("refused: %w", chat.ErrWouldDropText)
	}
	return p.Platform.Update(ctx, r, o)
}
