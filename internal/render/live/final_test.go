package live_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func (e *env) updates() int { return len(e.rec.sent("/chat.update")) }

// partial starts a live run and waits for its first post, which says "partial".
func (e *env) partial(t *testing.T) (*live.Run, context.CancelFunc) {
	t.Helper()
	run, cancel := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "partial"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	return run, cancel
}

// TestShutdownAfterTheFinalStillWritesTheFinalText: the session's context ends while the final
// text waits for its window. The task is over and nothing will send it again, so it is written.
func TestShutdownAfterTheFinalStillWritesTheFinalText(t *testing.T) {
	e := setup(t)
	run, cancel := e.partial(t)
	run.Handle(text("a", "partial, then the final answer"))
	run.Handle(agent.FinalEvent{})
	eventually(t, "the final to wait for its window", func() bool { return e.clk.waiting() == 1 })
	cancel()
	e.finish(t, run) // the context ended, and the run still writes the final when its window opens
	if got := e.reply().Text; got != "partial, then the final answer" {
		t.Fatalf("the message says %q after the shutdown; want the final text", got)
	}
}

// TestFinalFollowedAtOnceByTheCancelIsRetriedThroughARateLimit: drive returns right after the final
// event and the session cancels the turn's context. The first update is rate-limited; the run waits
// as Slack asked and the final text arrives, rather than being given up on after one attempt.
func TestFinalFollowedAtOnceByTheCancelIsRetriedThroughARateLimit(t *testing.T) {
	e := setup(t)
	e.rec.rateLimited = 1
	run, cancel := e.partial(t)
	run.Handle(text("a", "the final answer"))
	run.Handle(agent.FinalEvent{})
	cancel()
	eventually(t, "the final to wait for its window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the rate-limited request", func() bool { return e.updates() == 1 })
	eventually(t, "the wait for Retry-After", func() bool { return e.clk.waiting() == 1 })
	e.finish(t, run)
	if got := e.reply().Text; got != "the final answer" || e.updates() != 2 {
		t.Fatalf("the message says %q after %d updates; want the final text on the second", got, e.updates())
	}
}

// TestFinalIsGivenUpOnAtItsOwnDeadline: a final that cannot be written is tried for FinalDeadline and
// no longer, though the context of the turn ended long before.
func TestFinalIsGivenUpOnAtItsOwnDeadline(t *testing.T) {
	e := setup(t)
	e.rec.rateLimited = 1000 // Slack never lets it through
	run, cancel := e.partial(t)
	run.Handle(text("a", "the final answer"))
	run.Handle(agent.FinalEvent{})
	cancel()
	eventually(t, "the run to end at its deadline", func() bool {
		select {
		case <-run.Done():
			return true
		default:
		}
		if e.clk.waiting() > 0 {
			e.clk.advance(30 * time.Second)
		}
		return false
	})
	if !strings.Contains(e.logs.String(), "before its deadline") {
		t.Fatalf("log %q; want the missed deadline logged", e.logs.String())
	}
}

func TestFinalWithNothingToShowEndsTheRun(t *testing.T) {
	e := setup(t)
	// A message stands with a tool status line: it is not left up.
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != render.NoOutput || strings.Contains(string(m.Blocks), "Calling") {
		t.Fatalf("message %+v; want the placeholder, with no tool line", m)
	}
	// With nothing posted, the run just ends.
	again := setup(t)
	idle, _ := again.start(t, v1alpha1.OutputLive, false)
	idle.Handle(agent.FinalEvent{})
	select {
	case <-idle.Done():
	case <-timeoutAfter():
		t.Fatal("the run did not end for a turn with nothing to show")
	}
	if n := len(again.rec.sent("/chat.postMessage")) + again.updates(); n != 0 {
		t.Fatalf("%d requests for a turn with nothing to show", n)
	}
}

// TestRateLimitIsWaitedOutAndIsNoAttempt: Slack says to come back in 30 s. The write waits for exactly
// that, from the moment of the request, and does so more times than Attempts allows, because a rate
// limit is not a failure.
func TestRateLimitIsWaitedOutAndIsNoAttempt(t *testing.T) {
	e := setup(t)
	const limits = 7 // more than the default five attempts
	e.rec.rateLimited = limits
	run, _ := e.partial(t)
	run.Handle(text("a", "the final answer"))
	run.Handle(agent.FinalEvent{})
	eventually(t, "the final to wait for its window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle) // the window: the first request is rate-limited
	for i := 1; i <= limits; i++ {
		eventually(t, "the request", func() bool { return e.updates() == i })
		eventually(t, "the wait for Retry-After", func() bool { return e.clk.waiting() == 1 })
		if wait := e.clk.due().Sub(e.clk.Now()); wait != 30*time.Second {
			t.Fatalf("the write waits %v after a 429 with Retry-After 30 s; want 30 s", wait)
		}
		e.clk.advance(30 * time.Second)
	}
	e.finish(t, run)
	if e.reply().Text != "the final answer" || e.updates() != limits+1 {
		t.Fatalf("message %q after %d updates; want the final text after %d", e.reply().Text, e.updates(), limits+1)
	}
}

func TestGiveUpAfterAttemptsEndsTheRun(t *testing.T) {
	e := setup(t)
	e.rec.failUpdates = 100
	run, _ := e.partial(t)
	run.Handle(text("a", "the final answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if n := e.updates(); n != 5 || e.reply().Text != "partial" || !strings.Contains(e.logs.String(), "giving up") {
		t.Fatalf("%d updates, message %q, log %q; want five tries, then the give-up logged", n, e.reply().Text, e.logs.String())
	}
}

// failingSender is a Sender whose every post fails with err.
type failingSender struct {
	err   error
	calls int
}

func (s *failingSender) Send(context.Context, outbox.Post) (chat.Ref, error) {
	s.calls++
	return chat.Ref{}, s.err
}

func TestPermanentErrorEndsTheRunAtOnce(t *testing.T) {
	for name, permanent := range map[string]error{"invalid": store.ErrInvalid, "abandoned": outbox.ErrAbandoned} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			send := &failingSender{err: fmt.Errorf("deliver: %w", permanent)}
			logs := &strings.Builder{}
			r := live.New(live.Config{Sender: send, Platform: slack.NewPlatform(nil, "ns/conn"), Throttle: throttle, Now: e.clk.Now, After: e.clk.After, Logger: slog.New(slog.NewTextHandler(logs, nil))})
			run := r.Start(t.Context(), live.Turn{Source: "turn-1", Channel: "C1", ThreadTS: e.root, Mode: v1alpha1.OutputLive})
			run.Handle(text("a", "hello"))
			select {
			case <-run.Done():
			case <-timeoutAfter():
				t.Fatal("the run kept retrying a post that cannot succeed")
			}
			if send.calls != 1 || !strings.Contains(logs.String(), "cannot be sent") {
				t.Fatalf("%d calls, log %q; want one call and the give-up logged", send.calls, logs.String())
			}
		})
	}
}

// TestInterruptedFirstPostStillShowsTheFinalText: the first post reaches Slack but its answer is lost.
// The retry finds that message, which holds the first text, so the final text must be written to it.
func TestInterruptedFirstPostStillShowsTheFinalText(t *testing.T) {
	e := setup(t)
	e.rec.losePosts = 1
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "partial"))
	eventually(t, "the post to reach Slack", func() bool { return e.reply().TS != e.root && e.reply().Text == "partial" })
	run.Handle(text("a", "partial, then the final answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.reply().Text; got != "partial, then the final answer" {
		t.Fatalf("the message says %q; want the final text, not the first", got)
	}
}

// The same when the context ends first: the shutdown's write finds the first message too.
func TestInterruptedFirstPostIsSettledAtShutdown(t *testing.T) {
	e := setup(t)
	e.rec.losePosts = 1
	run, cancel := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "partial"))
	eventually(t, "the post to reach Slack", func() bool { return e.reply().TS != e.root && e.reply().Text == "partial" })
	eventually(t, "the retry to wait", func() bool { return e.clk.waiting() == 1 })
	run.Handle(text("a", "partial, then the final answer"))
	run.Handle(agent.FinalEvent{})
	cancel()
	e.finish(t, run)
	if got := e.reply().Text; got != "partial, then the final answer" {
		t.Fatalf("the message says %q after the shutdown; want the final text", got)
	}
}

func TestFallbackTextIsCapped(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", strings.Repeat("a", 1500)))
	eventually(t, "the post", func() bool { return len(e.reply().Blocks) > 0 && e.reply().TS != e.root })
	m := e.reply()
	if n := len(utf16.Encode([]rune(m.Text))); n > 1000 || !strings.HasSuffix(m.Text, "… (truncated)") || !strings.Contains(string(m.Blocks), strings.Repeat("a", 1500)) {
		t.Fatalf("fallback text of %d units %q; want it capped at 1000 with the blocks whole", n, m.Text)
	}
}

// TestEachFieldIsEscapedInItsOwnFlavour pushes hostile text through every field: the markdown
// block escapes brackets as well, the plain_text tool line and the fallback text do not.
func TestEachFieldIsEscapedInItsOwnFlavour(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "<!channel> [x](https://evil.example) & \x07 end"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "[t](https://evil.example)", Args: map[string]any{"v": "<!channel>"}})
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	eventually(t, "the tool line", func() bool { return strings.Contains(string(e.reply().Blocks), "Calling") })
	var blocks []struct {
		Type     string
		Text     string
		Elements []struct{ Type, Text string }
	}
	m := e.reply()
	if err := json.Unmarshal(m.Blocks, &blocks); err != nil || len(blocks) != 2 {
		t.Fatalf("blocks %s, %v; want a markdown block and a context block", m.Blocks, err)
	}
	for field, want := range map[string][3]string{
		"markdown block": {blocks[0].Text, `&lt;!channel&gt; \[x\](https://evil.example) &amp;  end`, "markdown"},
		"tool line":      {blocks[1].Elements[0].Text, `Calling [t](https://evil.example)(v="&lt;!channel&gt;")`, "mrkdwn"},
		"fallback text":  {m.Text, `&lt;!channel&gt; [x](https://evil.example) &amp;  end`, "text"},
	} {
		if want[0] != want[1] {
			t.Errorf("%s = %q; want %q (%s flavour)", field, want[0], want[1], want[2])
		}
	}
}

// TestFinalModeWaitsForTheFinal tells final mode from live: events and a turning clock bring no post
// until the final event, where live mode would have posted at once.
func TestFinalModeWaitsForTheFinal(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputFinal, false)
	run.Handle(text("a", "partial"))
	run.Handle(text("a", "the whole answer"))
	for range 20 { // turning the clock is a no-op here, as no timer is armed in final mode: it gives the goroutine time
		e.clk.advance(throttle)
		time.Sleep(time.Millisecond)
	}
	if n := len(e.rec.sent("/chat.postMessage")); n != 0 {
		t.Fatalf("%d posts before the final event", n)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if posts := len(e.rec.sent("/chat.postMessage")); posts != 1 || e.updates() != 0 || e.reply().Text != "the whole answer" {
		t.Fatalf("%d posts, %d updates, message %q; want one post of the whole answer", posts, e.updates(), e.reply().Text)
	}
}

// TestAHungWriteIsAbandonedAtWriteTimeout: Slack never answers an update. A write watches no context
// but its own timeout, which is all that bounds it; when it runs out the final is tried again, inside
// the final's deadline, and arrives.
//
// The timeout is real time, and it bounds the retry too: a retry that takes longer than the timeout is
// itself abandoned, and a third update follows. At 100 ms that happened to an ordinary request on a
// loaded machine (a third update, all three of the final text), so the timeout is long enough for the
// request that is not hung, and the test waits it out once, for the one that is.
func TestAHungWriteIsAbandonedAtWriteTimeout(t *testing.T) {
	e := setup(t)
	e.cfg.WriteTimeout = time.Second
	e.live = live.New(e.cfg)
	e.rec.hangUpdates = 1
	run, _ := e.partial(t)
	run.Handle(text("a", "the final answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.reply().Text; got != "the final answer" || e.updates() != 2 {
		t.Fatalf("the message says %q after %d updates; want the final text on the second, after the first was abandoned", got, e.updates())
	}
}
