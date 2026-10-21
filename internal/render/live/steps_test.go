package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/test/fakeslack"

	slackgo "github.com/slack-go/slack"
)

// chunksOf is the chunks of a streaming request.
func chunksOf(t *testing.T, f url.Values) []map[string]any {
	t.Helper()
	raw := f.Get("chunks")
	if raw == "" {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("chunks %s: %v", raw, err)
	}
	return out
}

// textOf is the text a streaming request carries, which is in its markdown_text chunks.
func textOf(f url.Values) string {
	var chunks []struct{ Type, Text string }
	_ = json.Unmarshal([]byte(f.Get("chunks")), &chunks)
	var b strings.Builder
	b.WriteString(f.Get("markdown_text"))
	for _, c := range chunks {
		if c.Type == "markdown_text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// timeline is the chunks a stream was sent, in order: "text:<what>" and "task:<id>:<status>".
func timeline(m fakeslack.Message) []string {
	var out []string
	for _, raw := range m.Chunks {
		var c struct{ Type, Text, ID, Status string }
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		switch c.Type {
		case "markdown_text":
			out = append(out, "text:"+c.Text)
		case "task_update":
			out = append(out, "task:"+c.ID+":"+c.Status)
		}
	}
	return out
}

// wire is every value of every request the renderer made to Slack, as one text.
func (e *env) wire() string {
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	var b strings.Builder
	for path, forms := range e.rec.forms {
		for _, f := range forms {
			b.WriteString(path + " " + f.Encode() + "\n")
		}
	}
	return b.String()
}

func call(id, name string, args map[string]any) agent.ToolCallEvent {
	return agent.ToolCallEvent{CallID: id, Name: name, Args: args}
}

func result(id string, response any) agent.ToolResultEvent {
	return agent.ToolResultEvent{CallID: id, Response: response}
}

// window lets the renderer write what it has waited to: it waits for the window, and turns the clock.
func (e *env) window(t *testing.T) {
	t.Helper()
	eventually(t, "the renderer to wait for its window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
}

func (e *env) tasks() []fakeslack.Task { return e.reply().Tasks() }

// TestAToolCallIsAStepThatIsDoneWhenItsResultComes: the call begins a step, which the stream starts with when nothing
// was said yet, and the result finishes it. The reply's text follows, and the stream stops with the step done.
func TestAToolCallIsAStepThatIsDoneWhenItsResultComes(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(call("c1", "kubectl_get", map[string]any{"ns": "prod"}))
	eventually(t, "the step to be shown", func() bool { return len(e.tasks()) == 1 })
	want := fakeslack.Task{ID: "s1", Title: "kubectl_get", Status: "in_progress", Details: `ns="prod"`}
	if got := e.tasks(); got[0] != want || !e.reply().Streaming {
		t.Fatalf("tasks %+v streaming %v; want %+v, and the stream running", got, e.reply().Streaming, want)
	}
	run.Handle(result("c1", map[string]any{"output": "3 pods"}))
	e.window(t)
	eventually(t, "the step to be done", func() bool { return e.tasks()[0].Status == "complete" })
	run.Handle(text("a", "Three pods."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	want.Status = "complete"
	if m.Streaming || m.Text != "Three pods." || len(m.Tasks()) != 1 || m.Tasks()[0] != want {
		t.Fatalf("after the stop: %+v tasks %+v; want the text, and the step done, with its details kept", m, m.Tasks())
	}
	if got, want := timeline(m), []string{"task:s1:in_progress", "task:s1:complete", "text:Three pods."}; !slices.Equal(got, want) {
		t.Fatalf("chunks %v; want %v", got, want)
	}
	e.assertInert(t)
}

// A result that says the call failed finishes the step as an error, and what the tool said of it is not shown.
func TestAStepIsAnErrorWhenItsResultSaysTheCallFailed(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(call("c1", "add_numbers", nil))
	eventually(t, "the step to be shown", func() bool { return len(e.tasks()) == 1 })
	run.Handle(result("c1", map[string]any{"error": "tool \"add_numbers\" call is rejected: <!channel> hunter2hunter2"}))
	e.window(t)
	eventually(t, "the step to be an error", func() bool { return e.tasks()[0].Status == "error" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.tasks()[0]; got.Status != "error" || got.Output != "" || got.Details != "" {
		t.Fatalf("task %+v; want an error with no output: the tool's words are the agent's and are not shown", got)
	}
	if w := e.wire(); strings.Contains(w, "rejected") || strings.Contains(w, "hunter2") {
		t.Fatalf("the tool's error reached Slack:\n%s", w)
	}
}

// Several calls are several steps, in the order of the calls, and each ends by its own result, whatever order the
// results come in. A step that is over before it is first shown is begun in its outcome, in one call. A window begins
// at most maxBegins steps, so the third comes with the next window.
func TestSeveralToolsAreStepsInOrderEachEndedByItsOwnResult(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	run := e.stream(t)
	run.Handle(call("c1", "first", nil))
	run.Handle(call("c2", "second", nil))
	run.Handle(call("c3", "third", nil))
	run.Handle(result("c2", map[string]any{"output": "x"}))
	run.Handle(result("c1", map[string]any{"error": "no"}))
	release()
	eventually(t, "the first two steps to be shown", func() bool { return len(e.tasks()) == 2 })
	e.window(t)
	eventually(t, "the third step to be shown", func() bool { return len(e.tasks()) == 3 })
	want := []fakeslack.Task{{ID: "s1", Title: "first", Status: "error"}, {ID: "s2", Title: "second", Status: "complete"}, {ID: "s3", Title: "third", Status: "in_progress"}}
	if got := e.tasks(); !slices.Equal(got, want) {
		t.Fatalf("tasks %+v; want %+v", got, want)
	}
	if n := len(e.rec.sent("/chat.appendStream")); n != 3 {
		t.Fatalf("%d appends; want one for each step, none for the state a step began in", n)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// The steps come where their calls were made: what was said before a call is in the stream before its step, and what
// came after is not.
func TestAStepComesWhereItsCallWasMadeInTheText(t *testing.T) {
	t.Run("between two texts", func(t *testing.T) {
		e := setup(t)
		run := e.stream(t)
		run.Handle(text("a", "Let me look."))
		eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
		run.Handle(call("c1", "search", nil))
		run.Handle(text("b", "Found it."))
		e.window(t)
		eventually(t, "the text after the step", func() bool { return e.reply().Text == "Let me look.\n\nFound it." })
		run.Handle(result("c1", map[string]any{"output": "x"}))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
		want := []string{"text:Let me look.", "task:s1:in_progress", "text:\n\nFound it.", "task:s1:complete"}
		if got := timeline(e.reply()); !slices.Equal(got, want) {
			t.Fatalf("chunks %v; want %v", got, want)
		}
	})
	t.Run("before any text", func(t *testing.T) {
		e := setup(t)
		release := e.hold()
		t.Cleanup(release)
		run := e.stream(t)
		run.Handle(call("c1", "search", nil))
		run.Handle(text("a", "Hello"))
		release()
		eventually(t, "the text", func() bool { return e.reply().Text == "Hello" })
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
		// The call has no result when the turn ends, so its step ends then, after the text.
		if got, want := timeline(e.reply()), []string{"task:s1:in_progress", "text:Hello", "task:s1:complete"}; !slices.Equal(got, want) {
			t.Fatalf("chunks %v; want %v", got, want)
		}
	})
	t.Run("a call made after all the text", func(t *testing.T) {
		e := setup(t)
		release := e.hold()
		t.Cleanup(release)
		run := e.stream(t)
		run.Handle(text("a", "Done."))
		run.Handle(call("c1", "audit", nil))
		run.Handle(result("c1", map[string]any{"output": "x"}))
		run.Handle(agent.FinalEvent{})
		release()
		e.finish(t, run)
		if got, want := timeline(e.reply()), []string{"text:Done.", "task:s1:complete"}; !slices.Equal(got, want) {
			t.Fatalf("chunks %v; want %v", got, want)
		}
	})
}

// A call with no result when the turn ends is a step whose outcome is not known. Slack has no such state, so the step is
// completed with words that say no result was seen, never shown as a plain success, and it is ended before the stop,
// which sends no blocks, as before.
func TestAToolWithNoResultAtTheStopIsEndedNotLeftRunningAndNotShownAsSuccess(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(call("c1", "slow_query", nil))
	eventually(t, "the step to be shown", func() bool { return len(e.tasks()) == 1 })
	run.Handle(text("a", "I could not wait for it."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	got := e.tasks()
	if len(got) != 1 || got[0].Status != "complete" || !strings.Contains(got[0].Output, "No result") {
		t.Fatalf("tasks %+v; want the step complete, with output that says no result was reported", got)
	}
	if m := e.reply(); m.Streaming || m.Text != "I could not wait for it." {
		t.Fatalf("after the stop: %+v", m)
	}
	paths := e.rec.orderOf()
	if last := paths[len(paths)-1]; last != "/chat.stopStream" {
		t.Fatalf("the last request is %s; want the stop, with the step ended before it", last)
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || stops[0].Has("blocks") || textOf(stops[0]) != "I could not wait for it." {
		t.Fatalf("stops %v; want one, with the last of the text and no blocks", stops)
	}
}

// What the agent wrote as a tool's name and arguments reaches Slack as characters, on one line, cleaned and capped:
// nothing in it is a mention, a link or a broadcast.
func TestStepTitlesAreCleanedEscapedAndCapped(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(call("c1", "<!channel>\n\u202e[x](https://evil.test)"+strings.Repeat("n", 500), map[string]any{
		"q": "<@U0123456789> [y](https://evil.test) " + strings.Repeat("v", 500), "<!here>": "\u200bz",
	}))
	eventually(t, "the step to be shown", func() bool { return len(e.tasks()) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	got := e.tasks()[0]
	for name, s := range map[string]string{"title": got.Title, "details": got.Details} {
		if strings.ContainsAny(s, "\n\r\u202e\u200b<>") || chat.Units(s) > 256 {
			t.Errorf("the %s %q is not one clean line of at most 256 units, with no markup", name, s)
		}
	}
	if !strings.HasPrefix(got.Title, `&lt;!channel&gt; [x]\(https://evil.test)`) {
		t.Errorf("title %q; want the name as text, escaped", got.Title)
	}
	e.assertInert(t)
}

// The arguments of a call are shown in a step as a summary with their secrets taken out, by the name they sit under and by
// the shape of what they hold (spec 8.4), as they are in the status line of the update path. Nothing that was taken out
// reaches Slack, in a step or in any request.
func TestTheArgumentsOfACallAreShownInAStepOnlyAsTheirRedactedSummary(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(call("c1", "run", map[string]any{
		"query": "up", "password": "hunter2hunter2", "token": "xoxb-123-456-abcdefghijkl",
		"url": "https://admin:s3cretpass@db.example/x", "cfg": map[string]any{"apiKey": "k-1234567890", "name": "db1"},
	}))
	eventually(t, "the step to be shown", func() bool { return len(e.tasks()) == 1 })
	run.Handle(result("c1", map[string]any{"output": "hunter2hunter2"}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if d := e.tasks()[0].Details; !strings.Contains(d, `query="up"`) || !strings.Contains(d, "db1") {
		t.Fatalf("details %q; want what is no secret to be shown", d)
	}
	w := e.wire()
	for _, secret := range []string{"hunter2hunter2", "xoxb-123", "s3cretpass", "k-1234567890"} {
		if strings.Contains(w, secret) {
			t.Errorf("a request holds %q:\n%s", secret, w)
		}
	}
}

// spy is a platform whose streams count the steps they are asked for, and may fail them: it has the capabilities it is
// given, whatever the adapter under it can do.
type spy struct {
	chat.Platform
	caps       chat.Caps
	steps      *atomic.Int32
	fail       func(n int) error // the error of the nth step, or nil
	streamFail error             // what Stream says, if it is not nil
	streamFn   func() error      // or this, which is asked at each Stream and says nil to let it through
}

func (s spy) Capabilities() chat.Caps { return s.caps }

func (s spy) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	if s.streamFail != nil {
		return nil, s.streamFail
	}
	if s.streamFn != nil {
		if err := s.streamFn(); err != nil {
			return nil, err
		}
	}
	st, err := s.Platform.Stream(ctx, t, to, md)
	return spyStream{Stream: st, spy: s}, err
}

func (s spy) Classify(op chat.Op, err error) chat.Verdict {
	if errors.Is(err, errRefused) {
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent}
	}
	return s.Platform.Classify(op, err)
}

type spyStream struct {
	chat.Stream
	spy spy
}

func (s spyStream) Step(ctx context.Context, st chat.Step) error {
	n := int(s.spy.steps.Add(1))
	if s.spy.fail != nil {
		if err := s.spy.fail(n); err != nil {
			return err
		}
	}
	return s.Stream.Step(ctx, st)
}

var errRefused = errors.New("invalid_chunks")

func (e *env) spy(t *testing.T, steps bool, fail func(int) error) *atomic.Int32 {
	t.Helper()
	n := new(atomic.Int32)
	caps := e.cfg.Platform.Capabilities()
	caps.Steps = steps
	e.cfg.Platform = spy{Platform: e.cfg.Platform, caps: caps, steps: n, fail: fail}
	e.live = newRenderer(t, e.cfg)
	return n
}

// A platform that has no steps is never asked for one: the turn streams its text as it did, and the calls are not shown.
func TestNoStepIsAskedOfAPlatformWithoutThem(t *testing.T) {
	e := setup(t)
	steps := e.spy(t, false, nil)
	run := e.stream(t)
	run.Handle(text("a", "Hello"))
	run.Handle(call("c1", "search", nil))
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(call("c2", "fetch", nil))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if n := steps.Load(); n != 0 {
		t.Fatalf("%d steps were asked of a platform with none", n)
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Fatalf("a platform with no steps drew a warning:\n%s", e.logs)
	}
	if m := e.reply(); m.Streaming || m.Text != "Hello" || len(m.Tasks()) != 0 {
		t.Fatalf("message %+v tasks %+v; want the text, streamed and stopped, and no step", m, m.Tasks())
	}
	if e.updates() != 0 {
		t.Fatal("an update was made for a tool call on a stream: it has no status line")
	}
}

// A stream with no steps and a tool call alone has nothing to show, and is not started on nothing.
func TestAToolCallAloneStartsNoStreamOnAPlatformWithoutSteps(t *testing.T) {
	e := setup(t)
	e.spy(t, false, nil)
	run := e.stream(t)
	run.Handle(call("c1", "search", nil))
	run.Handle(agent.FinalEvent{})
	select {
	case <-run.Done():
	case <-timeoutAfter():
		t.Fatal("the run did not end")
	}
	if n := len(e.rec.sent("/chat.startStream")) + len(e.rec.sent("/chat.postMessage")); n != 0 {
		t.Fatalf("%d messages started for a tool call on a platform with no steps", n)
	}
}

// A step is decoration: a platform that refuses one for good, or that says it has none after all, ends the steps
// of the turn, and the reply is streamed and stopped as if there had been none.
func TestAPlatformThatRefusesAStepForGoodEndsTheStepsAndNotTheReply(t *testing.T) {
	for name, refuse := range map[string]error{"refused": fmt.Errorf("append: %w", errRefused), "no steps": chat.ErrNoSteps} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			steps := e.spy(t, true, func(int) error { return refuse })
			run := e.stream(t)
			run.Handle(text("a", "Hel"))
			run.Handle(call("c1", "search", nil))
			eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
			run.Handle(text("b", "lo"))
			run.Handle(call("c2", "fetch", nil))
			e.window(t)
			eventually(t, "the text", func() bool { return e.reply().Text == "Hel\n\nlo" })
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if m := e.reply(); m.Streaming || m.Text != "Hel\n\nlo" || len(m.Tasks()) != 0 {
				t.Fatalf("message %+v; want the whole text, stopped, and no step", m)
			}
			if n := steps.Load(); n != 1 {
				t.Fatalf("%d steps were asked for; want one, which was refused for good: the turn goes on without steps", n)
			}
			if warned := strings.Contains(e.logs.String(), "level=WARN"); warned != (name == "refused") {
				t.Errorf("warned %t for %s; want a warning for a step refused for good, and none for a stream that has no steps:\n%s", warned, name, e.logs)
			}
		})
	}
}

// A step that fails in a way that may pass is tried again with the next change, and the text goes on in the meantime.
func TestAStepThatFailsForNowIsTriedAgainAndTheTextGoesOn(t *testing.T) {
	e := setup(t)
	steps := e.spy(t, true, func(n int) error {
		if n == 1 {
			return errors.New("connection reset")
		}
		return nil
	})
	release := e.hold() // the first write is not throttled: text and call must be in before it, or the step waits for a window nobody turns
	t.Cleanup(release)
	run := e.stream(t)
	run.Handle(text("a", "Hel"))
	run.Handle(call("c1", "search", nil))
	release()
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	eventually(t, "the failed step to have been asked for", func() bool { return steps.Load() == 1 })
	run.Handle(text("b", "lo"))
	e.window(t)
	eventually(t, "the step and the text", func() bool { return len(e.tasks()) == 1 && e.reply().Text == "Hel\n\nlo" })
	if n := steps.Load(); n != 2 {
		t.Fatalf("%d steps asked for; want the one that failed, and its retry", n)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// A step that never succeeds is let go at the final, which does not wait on it: the stop is sent with the text.
func TestAFailingStepNeverHoldsBackTheStop(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	steps := e.spy(t, true, func(int) error { return errors.New("connection reset") })
	run := e.stream(t)
	run.Handle(text("a", "Done."))
	run.Handle(call("c1", "search", nil))
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)
	if m := e.reply(); m.Streaming || m.Text != "Done." || m.Metadata == nil {
		t.Fatalf("message %+v; want the text, stopped and tagged, whatever became of the step", m)
	}
	if steps.Load() == 0 {
		t.Fatal("the step was not tried")
	}
}

// streamedThenLost is an earlier run of the test's turn that streamed text and began the step of a call, and was lost
// (its context ended, as a broker that stops ends it) with the stream open and its ts recorded.
func (e *env) streamedThenLost(t *testing.T) {
	t.Helper()
	release := e.hold() // the first write is not throttled: text and call must be in before it, or the step waits for a window nobody turns
	t.Cleanup(release)
	first, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	first.Handle(text("a", "partial"))
	first.Handle(call("c1", "search", nil))
	release()
	eventually(t, "the step and the ts to be recorded", func() bool {
		row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply"))
		return err == nil && row.MessageRef != "" && len(e.tasks()) == 1
	})
	cancel()
	<-first.Done()
}

// A run that takes over the stream of an earlier run ends that stream and updates the message, and the update carries
// every step the run has, since Slack drops a stream's cards from a message it updates: the steps of the earlier run are
// shown once, by the update, and none is added to the stream. While the run goes on its updates show the steps as they
// stand, and at the end a step with no result is ended, as a stream's is at its stop.
func TestARecoveredTurnKeepsTheStepsOfTheStreamItTakesOver(t *testing.T) {
	e := setup(t)
	e.streamedThenLost(t)
	appends := len(e.rec.sent("/chat.appendStream"))

	release := e.hold()
	t.Cleanup(release)
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(text("a", "the whole"))
	again.Handle(call("c1", "search", map[string]any{"q": "up"})) // the earlier run's call, said again
	again.Handle(result("c1", map[string]any{"output": "x"}))
	again.Handle(call("c2", "fetch", nil))
	release()
	eventually(t, "an update that shows the steps as they stand", func() bool {
		got := e.tasks()
		return len(got) == 2 && got[0].Status == "complete" && got[1].Status == "in_progress" && !e.reply().Streaming
	})
	if blocks := e.reply().Blocks; strings.Contains(string(blocks), "Calling") {
		t.Errorf("blocks %s; want no status line beside the cards, which say the same", blocks)
	}
	again.Handle(text("a", "the whole answer"))
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)

	th := e.fake.Thread("C1", e.root)
	if len(th) != 2 || th[1].Streaming || th[1].Text != "the whole answer" {
		t.Fatalf("thread %+v; want one message with the whole text, no longer streaming", th)
	}
	if got := e.rec.sent("/chat.appendStream"); len(got) != appends {
		t.Fatalf("%d appends, %d before the takeover; want the takeover to append nothing, steps included", len(got), appends)
	}
	want := []fakeslack.Task{
		{ID: "s1", Title: "search", Status: "complete", Details: `q="up"`},
		{ID: "s2", Title: "fetch", Status: "complete", Output: "No result was reported."},
	}
	if got := th[1].Tasks(); !slices.Equal(got, want) {
		t.Fatalf("tasks %+v; want %+v: every step of the run, the one with no result ended", got, want)
	}
	if got := strings.Join(blockTypes(t, th[1].Blocks), ","); got != "task_card,task_card,markdown" {
		t.Errorf("blocks %s; want the cards before the text", got)
	}
	e.assertLastStopIsBare(t)
}

// blockTypes is the type of each block of raw, in order.
func blockTypes(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("blocks %s: %v", raw, err)
	}
	var out []string
	for _, b := range blocks {
		out = append(out, b.Type)
	}
	return out
}

// A stream whose text diverged is stopped and replaced by an update, which carries its steps: Slack would drop the
// cards the stream showed with the text it replaces.
func TestAStreamThatDivergedKeepsItsStepsInTheUpdateThatReplacesIt(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	run := e.stream(t)
	run.Handle(text("a", "The answer is 41"))
	run.Handle(call("c1", "search", nil))
	run.Handle(result("c1", map[string]any{"output": "x"}))
	release()
	eventually(t, "the step", func() bool { got := e.tasks(); return len(got) == 1 && got[0].Status == "complete" })
	run.Handle(text("a", "The answer is 42")) // not an extension of what was streamed
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if m.Streaming || m.Text != "The answer is 42" || len(e.rec.sent("/chat.update")) != 1 {
		t.Fatalf("message %+v after %d updates; want the final text, by one update after the stop", m, len(e.rec.sent("/chat.update")))
	}
	if got := m.Tasks(); len(got) != 1 || got[0].ID != "s1" || got[0].Status != "complete" {
		t.Fatalf("tasks %+v; want the stream's step, kept by the update", got)
	}
}

// refusesSteps is a platform that refuses an update with steps as Slack refuses blocks it cannot take.
type refusesSteps struct {
	chat.Platform
	refused *atomic.Int32
}

func (p refusesSteps) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	if o.Message != nil && slices.ContainsFunc(o.Message.Parts, func(p chat.Part) bool { _, ok := p.(chat.Steps); return ok }) {
		p.refused.Add(1)
		return slackgo.SlackErrorResponse{Err: "invalid_blocks"}
	}
	return p.Platform.Update(ctx, r, o)
}

// The steps never cost the reply its text: an update that the platform refuses as content it cannot show is sent again
// without them, at once, with a warning, and the turn shows no steps from then on.
func TestAnUpdateRefusedWithItsStepsIsSentAgainWithout(t *testing.T) {
	e := setup(t)
	e.streamedThenLost(t)
	refused := new(atomic.Int32)
	e.cfg.Platform = refusesSteps{Platform: e.cfg.Platform, refused: refused}
	e.live = newRenderer(t, e.cfg)
	release := e.hold()
	t.Cleanup(release)
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(text("a", "the whole answer"))
	again.Handle(call("c1", "search", nil))
	again.Handle(result("c1", map[string]any{"output": "x"}))
	again.Handle(agent.FinalEvent{})
	release()
	e.finish(t, again)
	m := e.reply()
	if m.Streaming || m.Text != "the whole answer" || len(m.Tasks()) != 0 {
		t.Fatalf("message %+v, tasks %+v; want the whole text, and no card", m, m.Tasks())
	}
	if n := refused.Load(); n != 1 {
		t.Errorf("%d updates with steps; want the one that was refused, and none after it", n)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") || !strings.Contains(e.logs.String(), "without them") {
		t.Errorf("logs:\n%s\nwant a warning that the message was sent without its steps", e.logs)
	}
}

// The renderer of a FakePlatform with every capability shows steps through the neutral seam alone, with no Slack under it.
func TestStepsAreShownThroughTheNeutralSeamOfAnyPlatform(t *testing.T) {
	e := setup(t)
	fake := chattest.NewFake()
	e.cfg.Platform = fake
	e.live = newRenderer(t, e.cfg)
	run := e.stream(t)
	run.Handle(call("c1", "search", map[string]any{"q": "up"}))
	eventually(t, "the step", func() bool { m := fake.Messages(); return len(m) == 1 && len(m[0].Steps) == 1 })
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(text("a", "ok"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := fake.Messages()[0]
	want := []chat.Step{{ID: "s1", Title: "search", Detail: `q="up"`, State: chat.StepDone}}
	if !slices.Equal(m.Steps, want) || m.Streaming || m.Streamed != "ok" {
		t.Fatalf("message %+v; want the step %+v, done, and the text", m, want)
	}
}
