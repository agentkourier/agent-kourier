package live_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
)

// If the second of three steps cannot be shown at the final, in a way that may pass, the stop does not wait for it and
// the steps after it are still ended: the first and the third are shown ended, and the stop is sent.
func TestAStepThatFailsAtTheFinalIsLetGoAndTheStepsAfterItAreStillEnded(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	steps := e.spy(t, true, func(n int) error {
		if n == 2 {
			return errors.New("connection reset")
		}
		return nil
	})
	run := e.stream(t)
	run.Handle(call("c1", "first", nil))
	run.Handle(call("c2", "second", nil))
	run.Handle(call("c3", "third", nil))
	run.Handle(text("a", "Done."))
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)
	if n := steps.Load(); n != 3 {
		t.Fatalf("%d steps asked for; want all three, the second of which failed", n)
	}
	m := e.reply()
	if m.Streaming || m.Text != "Done." || m.Metadata == nil {
		t.Fatalf("message %+v; want the stop, with the text and the tag", m)
	}
	var shown []string
	for _, tk := range m.Tasks() {
		shown = append(shown, tk.ID+":"+tk.Status)
	}
	if want := []string{"s1:complete", "s3:complete"}; !slices.Equal(shown, want) {
		t.Fatalf("tasks %v; want the first and the third, ended, and none for the second", shown)
	}
	paths := e.rec.orderOf()
	if paths[len(paths)-1] != "/chat.stopStream" {
		t.Fatalf("the last request is %s; want the stop", paths[len(paths)-1])
	}
}

// A window begins at most two steps, and the text waits for the ones it has not begun: it is never sent ahead of a step
// that came before it. The next window goes on, with no event to wake it.
func TestAWindowBeginsAtMostTwoStepsAndTheTextWaitsForTheRest(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	run := e.stream(t)
	run.Handle(text("a", "A"))
	for i := 1; i <= 5; i++ {
		run.Handle(call(fmt.Sprint("c", i), "step", nil))
	}
	run.Handle(text("b", "B"))
	release()
	eventually(t, "the first two steps", func() bool { return len(e.tasks()) == 2 })
	if got := e.reply().Text; got != "A" {
		t.Fatalf("the text is %q with steps still to begin; want it held at the text that came before them", got)
	}
	e.window(t)
	eventually(t, "the next two steps", func() bool { return len(e.tasks()) == 4 })
	e.window(t)
	eventually(t, "the last step, and the text after it", func() bool { return len(e.tasks()) == 5 && e.reply().Text == "A\n\nB" })
	want := []string{"text:A", "task:s1:in_progress", "task:s2:in_progress", "task:s3:in_progress", "task:s4:in_progress", "task:s5:in_progress", "text:\n\nB"}
	if got := timeline(e.reply()); !slices.Equal(got, want) {
		t.Fatalf("chunks %v; want %v", got, want)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// A step that is over is finished whatever the window's count of the ones it begins, and the final begins every step that is
// left: it is the last window there is.
func TestStepsThatAreOverAreFinishedAtOnceAndTheFinalBeginsTheRest(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	for i := 1; i <= 2; i++ {
		run.Handle(call(fmt.Sprint("c", i), "step", nil))
	}
	eventually(t, "the two steps", func() bool { return len(e.tasks()) == 2 })
	for i := 3; i <= 6; i++ {
		run.Handle(call(fmt.Sprint("c", i), "step", nil))
	}
	run.Handle(result("c1", map[string]any{"output": "x"}))
	e.window(t) // begins two of the four, and finishes the first
	eventually(t, "the next steps", func() bool { return len(e.tasks()) == 4 && e.tasks()[0].Status == "complete" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.tasks(); len(got) != 6 || got[5].Status != "complete" || !strings.Contains(got[5].Output, "No result") {
		t.Fatalf("tasks %+v; want all six, the last ended", got)
	}
}

// A step that meets a rate limit waits as long as Slack asked, as any write does, and the limit is no strike against the
// steps: more of them than the turn is given attempts at are still shown once Slack lets them through.
func TestAStepThatIsRateLimitedWaitsAndIsNoStrikeAgainstTheSteps(t *testing.T) {
	e := setup(t)
	limited := &slackgo.RateLimitedError{RetryAfter: 30 * time.Second}
	steps := e.spy(t, true, func(n int) error {
		if n <= 7 { // more than the Attempts of the turn, which is five
			return limited
		}
		return nil
	})
	run := e.stream(t)
	run.Handle(text("a", "Hel"))
	run.Handle(call("c1", "search", nil))
	eventually(t, "the step to be shown, after the waits", func() bool {
		if e.clk.waiting() > 0 {
			e.clk.advance(30 * time.Second)
		}
		return len(e.tasks()) == 1
	})
	if n := steps.Load(); n != 8 {
		t.Fatalf("%d steps asked for; want seven that were limited, and the one that went through", n)
	}
	if l := e.logs.String(); strings.Contains(l, "giving up on steps") || strings.Contains(l, "refused a step") {
		t.Fatalf("a rate limit ended the steps:\n%s", l)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Streaming || m.Text != "Hel" || len(m.Tasks()) != 1 {
		t.Fatalf("message %+v; want the text, stopped, and the step", m)
	}
}

// A platform that refuses the stream for good does not cost the reply: it is posted through the outbox, under a row
// of its own, and nothing was started as a stream.
func TestAPlatformThatRefusesTheStreamForGoodGetsThePostInstead(t *testing.T) {
	e := setup(t)
	e.cfg.Platform = spy{Platform: e.cfg.Platform, caps: e.cfg.Platform.Capabilities(), steps: new(atomic.Int32), streamFail: fmt.Errorf("start: %w", errRefused)}
	e.live = live.New(e.cfg)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "Hello"))
	run.Handle(call("c1", "search", nil))
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	th := e.fake.Thread("C1", e.root)
	if len(th) != 2 || th[1].Streaming || th[1].Text != "Hello world" {
		t.Fatalf("thread %+v; want one message with the whole reply", th)
	}
	if n := len(e.rec.sent("/chat.startStream")); n != 0 {
		t.Fatalf("%d streams started", n)
	}
	if _, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1/recovered", "reply")); err != nil {
		t.Fatalf("the row of the post: %v", err)
	}
	if !strings.Contains(e.logs.String(), "refused the stream for good") {
		t.Fatalf("no warning of it:\n%s", e.logs)
	}
}

// A Slack that refuses the chunks mode, through the whole stack: the reply still streams, in markdown_text mode, with no steps,
// and the refusal is one warning.
func TestAReplyStillStreamsWhenSlackRefusesTheChunksMode(t *testing.T) {
	e := setup(t)
	adapterLog := &bytes.Buffer{}
	e.cfg.Platform = slack.NewPlatform(e.client, "ns/conn").WithLogger(slog.New(slog.NewTextHandler(adapterLog, nil)))
	e.live = live.New(e.cfg)
	e.fake.RefuseChunkMode("invalid_chunks")
	run := e.stream(t)
	run.Handle(text("a", "Hel"))
	run.Handle(call("c1", "search", nil))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 2 })
	run.Handle(text("b", "lo"))
	e.window(t)
	eventually(t, "the text", func() bool { return e.reply().Text == "Hel\n\nlo" })
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if m.Streaming || m.Text != "Hel\n\nlo" || len(m.Tasks()) != 0 || len(m.Chunks) != 0 {
		t.Fatalf("message %+v; want the whole text, streamed in markdown_text mode and stopped, and no step", m)
	}
	if n := strings.Count(adapterLog.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings from the adapter, want one:\n%s", n, adapterLog)
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Fatalf("the renderer warned of a stream that simply shows no steps:\n%s", e.logs)
	}
}

// At the final a step's rate limit is a failure like any other: the step is let go, and the stop is sent. A wait for every
// step that is limited would outlast the FinalDeadline, at the few calls a minute Slack allows, and cost the stop and the
// last of the text it carries.
func TestAStepsRateLimitAtTheFinalDoesNotHoldBackTheStop(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	limited := &slackgo.RateLimitedError{RetryAfter: 30 * time.Second}
	steps := e.spy(t, true, func(int) error { return limited })
	run := e.stream(t)
	run.Handle(text("a", "Done."))
	for i := 1; i <= 3; i++ {
		run.Handle(call(fmt.Sprint("c", i), "step", nil))
	}
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)
	if n := steps.Load(); n != 3 {
		t.Fatalf("%d steps asked for; want each of the three once: the run did not wait on a limit and try a step again", n)
	}
	m := e.reply()
	if m.Streaming || m.Text != "Done." || m.Metadata == nil || len(m.Tasks()) != 0 {
		t.Fatalf("message %+v tasks %+v; want the stop, with the text and the tag, and no card", m, m.Tasks())
	}
}

// A stream that fails for now is tried again and is not posted instead: the post is for a stream that can never
// start, and a post beside a stream that then starts would be the reply twice.
func TestATransientFailureOfTheStreamIsRetriedAndNeverPosted(t *testing.T) {
	e := setup(t)
	var starts atomic.Int32
	e.cfg.Platform = spy{Platform: e.cfg.Platform, caps: e.cfg.Platform.Capabilities(), steps: new(atomic.Int32), streamFn: func() error {
		if starts.Add(1) == 1 {
			return errors.New("connection reset")
		}
		return nil
	}}
	e.live = live.New(e.cfg)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "Hello"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	th := e.fake.Thread("C1", e.root)
	if len(th) != 2 || th[1].Streaming || th[1].Text != "Hello" {
		t.Fatalf("thread %+v; want one message, the stream, with the reply", th)
	}
	if s, p := len(e.rec.sent("/chat.startStream")), len(e.rec.sent("/chat.postMessage")); s != 1 || p != 0 || starts.Load() != 2 {
		t.Fatalf("%d streams started, %d posts, %d tries; want one stream after the retry and no post", s, p, starts.Load())
	}
}
