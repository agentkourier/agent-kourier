package live_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func (e *env) stream(t *testing.T) *live.Run {
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	return run
}

// assertInert fails if any message-sending request so far turned unfurling on, set link_names or
// asked for parse=full.
func (e *env) assertInert(t *testing.T) {
	t.Helper()
	for _, path := range []string{"/chat.postMessage", "/chat.update", "/chat.startStream", "/chat.appendStream", "/chat.stopStream"} {
		for _, f := range e.rec.sent(path) {
			if f.Get("unfurl_links") != "false" || f.Get("unfurl_media") != "false" || f.Has("link_names") || f.Get("parse") == "full" {
				t.Errorf("%s request is not inert: %v", path, f)
			}
		}
	}
}

// assertLastStopIsBare fails unless the last chat.stopStream request carried no blocks and no text. A run that
// takes over the stream of an earlier run only ends it, and the update that follows writes the message: Slack
// shows a stop's blocks under the streamed text, so blocks sent here would be shown as well as the update's.
func (e *env) assertLastStopIsBare(t *testing.T) {
	t.Helper()
	stops := e.rec.sent("/chat.stopStream")
	if len(stops) == 0 {
		t.Fatal("no chat.stopStream request was sent")
	}
	if last := stops[len(stops)-1]; last.Has("blocks") || last.Has("markdown_text") || last.Has("chunks") {
		t.Errorf("the stop of the earlier run's stream carried blocks %v and text %q; want neither", last["blocks"], textOf(last))
	}
}

func TestStreamRecordsItsTsAtOnceAndAppendsOnlyTheSuffix(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(text("a", "Hel"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	// R16: the ts is in the outbox row before anything is appended.
	eventually(t, "the ts to be recorded", func() bool {
		row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply"))
		return err == nil && row.MessageRef != "" && row.Payload == ""
	})
	if n := len(e.rec.sent("/chat.appendStream")); n != 0 {
		t.Fatalf("%d appends before the ts was recorded", n)
	}
	run.Handle(text("a", "Hello"))
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	run.Handle(text("a", "Hello wor"))
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"}) // a step, and no tool line
	e.clk.advance(throttle)
	eventually(t, "the append and the step", func() bool { return len(e.rec.sent("/chat.appendStream")) == 2 })
	// The text goes first, as only its new suffix, and then the step that was begun after it.
	if got := e.rec.sent("/chat.appendStream"); len(got) != 2 || textOf(got[0]) != "lo wor" || len(chunksOf(t, got[1])) != 1 || chunksOf(t, got[1])[0]["type"] != "task_update" {
		t.Fatalf("appends %v; want the new suffix of the text, and then the step", got)
	}
	if strings.Contains(string(e.reply().Blocks), "Calling") || len(e.rec.sent("/chat.update")) != 0 {
		t.Fatalf("a tool line or an update reached the stream: %+v", e.reply())
	}
	e.assertInert(t)
}

// TestStreamStopsWithTheTagAndNoBlocks: Slack appends what a stop sends under the text that was streamed, so
// a stop that carried the reply again would show it twice (found in dogfood, 2026-10-03). The stop carries the
// tag and the unsent suffix of the text, and nothing else: the message ends with its text once.
func TestStreamStopsWithTheTagAndNoBlocks(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(text("a", "Hello"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if m.Streaming || m.Text != "Hello world" || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "turn-1/reply" {
		t.Fatalf("after the stop: %+v; want the whole text and the outbox tag", m)
	}
	if n := strings.Count(string(m.Blocks), "Hello world"); n != 1 {
		t.Fatalf("the message has %d copies of its text in blocks %s; want exactly one", n, m.Blocks)
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || textOf(stops[0]) != " world" || stops[0].Has("blocks") {
		t.Fatalf("stops %v; want one, with the unsent suffix as its text and no blocks", stops)
	}
	if n := len(e.rec.sent("/chat.update")); n != 0 {
		t.Fatalf("%d updates for a stream that did not diverge", n)
	}
	e.assertInert(t)
}

func TestDivergentFinalIsCorrectedAfterTheStop(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(text("a", "Hello there"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Goodbye")) // not an extension of what was streamed
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	e.clk.advance(throttle)
	run.Handle(text("a", "Goodbye!")) // the next change waits for a new window: the divergence has been seen
	eventually(t, "the next window", func() bool { return e.clk.waiting() == 1 })
	run.Handle(text("a", "Goodbye, all"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Streaming || m.Text != "Goodbye, all" || strings.Count(string(m.Blocks), "Goodbye, all") != 1 || strings.Contains(string(m.Blocks), "Hello there") {
		t.Fatalf("after the stop: %+v blocks %s; want the final text once, and none of what was streamed", m, m.Blocks)
	}
	if stops := e.rec.sent("/chat.stopStream"); len(stops) != 1 || stops[0].Has("blocks") || textOf(stops[0]) != "" {
		t.Fatalf("stops %v; want one that carries no blocks and no text, which the update replaces", stops)
	}
	var kinds []string
	for _, p := range e.rec.orderOf() {
		if strings.HasPrefix(p, "/chat.") && p != "/chat.postMessage" {
			kinds = append(kinds, p)
		}
	}
	if want := []string{"/chat.startStream", "/chat.stopStream", "/chat.update"}; !slices.Equal(kinds, want) {
		t.Fatalf("requests %v; want %v: no append of a text that cannot be appended, and the update after the stop", kinds, want)
	}
}

// TestCrashBetweenStartAndRecordLeavesOneOrphan: the process dies after startStream returned and
// before the ts was recorded. The stream is left (R16), and the recovered turn posts the whole
// text as a new message. Nothing is appended to the orphan.
func TestCrashBetweenStartAndRecordLeavesOneOrphan(t *testing.T) {
	e := setup(t)
	e.rf.fail.Store(true)
	run, crash := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "partial"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	eventually(t, "the failed record to be waiting", func() bool { return e.clk.waiting() == 1 })
	crash()
	<-run.Done()
	if row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply")); err != nil || row.MessageRef != "" {
		t.Fatalf("outbox row %+v, %v; want a row with no ts", row, err)
	}
	e.rf.fail.Store(false)
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(text("a", "the whole answer"))
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)
	th := e.fake.Thread("C1", e.root)
	if len(th) != 3 || !th[1].Streaming || th[1].Text != "partial" || th[2].Text != "the whole answer" || len(e.rec.sent("/chat.appendStream")) != 0 {
		t.Fatalf("thread %+v; want the orphan stream, untouched, and one new complete message", th)
	}
	if _, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1/recovered", "reply")); err != nil {
		t.Fatalf("the recovered turn's row: %v", err)
	}
}

func TestStreamWithNothingToShowStartsNothing(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(agent.FinalEvent{})
	select {
	case <-run.Done():
	case <-timeoutAfter():
		t.Fatal("the run did not end for a turn with nothing to show")
	}
	if n := len(e.rec.sent("/chat.startStream")) + len(e.rec.sent("/chat.postMessage")); n != 0 {
		t.Fatalf("%d messages started for a turn with nothing to show", n)
	}
}

// TestShutdownAfterTheFinalStopsTheStream: the context ends while the final waits for its window,
// and the stream is still stopped, with its text and tag, rather than left running.
func TestShutdownAfterTheFinalStopsTheStream(t *testing.T) {
	e := setup(t)
	run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "Hello"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	eventually(t, "the final to wait for its window", func() bool { return e.clk.waiting() == 1 })
	cancel()
	e.finish(t, run)
	if m := e.reply(); m.Streaming || m.Text != "Hello world" || m.Metadata == nil {
		t.Fatalf("after the shutdown: %+v; want the stream stopped with the whole text and the tag", m)
	}
}

// TestRecoveredTurnWithItsTsStopsTheStreamAndUpdatesIt: a stream's ts was recorded, so the message
// exists and is Agent Kourier's. A turn that continues it, after a restart or a lost connection, ends the
// stream if it still runs (and goes on if it has ended) and writes the whole text to the same
// message: no second message, which is only accepted in the window before the ts is recorded.
func TestRecoveredTurnWithItsTsStopsTheStreamAndUpdatesIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstEnded   bool
		wantStopReqs int
	}{{"the stream still runs", false, 1}, {"the stream has ended", true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			first, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
			first.Handle(text("a", "partial"))
			eventually(t, "the stream's ts to be recorded", func() bool {
				row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply"))
				return err == nil && row.MessageRef != ""
			})
			if tc.firstEnded {
				first.Handle(agent.FinalEvent{})
				e.finish(t, first)
			} else {
				cancel()
				<-first.Done()
			}
			again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
			again.Handle(text("a", "the whole answer"))
			again.Handle(agent.FinalEvent{})
			e.finish(t, again)
			th := e.fake.Thread("C1", e.root)
			if len(th) != 2 || th[1].Streaming || th[1].Text != "the whole answer" || len(e.rec.sent("/chat.startStream")) != 1 || len(e.rec.sent("/chat.stopStream")) != tc.wantStopReqs {
				t.Fatalf("thread %+v, %d starts, %d stops; want one message with the whole text, started once, stopped %d times", th, len(e.rec.sent("/chat.startStream")), len(e.rec.sent("/chat.stopStream")), tc.wantStopReqs)
			}
			e.assertLastStopIsBare(t)
		})
	}
}

// TestFinalModeWithARecipientPostsOnce: the stream is for live output. In final mode the person who
// asked is not a reason to stream; the answer is posted once, when it is whole.
func TestFinalModeWithARecipientPostsOnce(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputFinal, Recipient: "U1"})
	run.Handle(text("a", "partial"))
	run.Handle(text("a", "the whole answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if starts, posts := len(e.rec.sent("/chat.startStream")), len(e.rec.sent("/chat.postMessage")); starts != 0 || posts != 1 || e.reply().Text != "the whole answer" {
		t.Fatalf("%d streams, %d posts, message %q; want one post and no stream", starts, posts, e.reply().Text)
	}
}

// TestRowWithNoTsPostsAnew: a row of this turn exists with no ts, so a stream may exist that cannot
// be found. Whoever finds the row, a recovered turn or not, posts the whole text as a new message
// under a row of its own, and starts no stream.
func TestRowWithNoTsPostsAnew(t *testing.T) {
	e := setup(t)
	row := store.OutboxEntry{ID: outbox.ID("turn-1", "reply"), EventID: "turn-1", Role: store.OutboxReply, Channel: "C1", Thread: e.root, CreatedAt: e.clk.Now()}
	if _, err := e.store.CreateOutbox(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	run := e.stream(t)
	run.Handle(text("a", "the whole answer"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if _, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1/recovered", "reply")); err != nil || len(e.rec.sent("/chat.startStream")) != 0 || e.reply().Text != "the whole answer" {
		t.Fatalf("row %v, %d streams, message %q; want a new post under its own row and no stream", err, len(e.rec.sent("/chat.startStream")), e.reply().Text)
	}
}

// TestCancelDuringTheStartDoesNotOrphanAStream: the turn's context ends while startStream is in
// flight. The write does not watch that context, so it completes: the stream is recorded, and the
// run, which never saw a final, then ends. A cut request would leave a stream nobody holds the ts of.
func TestCancelDuringTheStartDoesNotOrphanAStream(t *testing.T) {
	e := setup(t)
	e.rec.holdStart = make(chan struct{})
	run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
	run.Handle(text("a", "hello"))
	eventually(t, "the start to be in flight", func() bool { return e.rec.holding() })
	cancel()
	close(e.rec.holdStart)
	select {
	case <-run.Done():
	case <-timeoutAfter():
		t.Fatal("the run did not end")
	}
	row, err := e.store.GetOutbox(t.Context(), outbox.ID("turn-1", "reply"))
	th := e.fake.Thread("C1", e.root)
	if len(e.rec.sent("/chat.startStream")) != 1 || len(th) != 2 || err != nil || row.MessageRef != th[1].TS {
		t.Fatalf("%d starts, thread %d long, row %+v, %v; want one stream, and its ts recorded", len(e.rec.sent("/chat.startStream")), len(th), row, err)
	}
}

// stopFails is a Platform whose streams of earlier runs stop with err, what the adapter says to a stop.
type stopFails struct {
	chat.Platform
	err error
}

func (s stopFails) StreamAt(r chat.Ref) chat.Stream {
	return stoppingFails{Stream: s.Platform.StreamAt(r), err: s.err}
}

type stoppingFails struct {
	chat.Stream
	err error
}

func (s stoppingFails) Stop(context.Context, string, chat.Message, string) error { return s.err }

// postedByUpdate runs a turn on the update path (no recipient), posts its message and abandons it, so
// that a recovered turn with a recipient finds the row of a message that was never a stream.
func (e *env) postedByUpdate(t *testing.T) {
	t.Helper()
	first, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
	first.Handle(text("a", "partial"))
	eventually(t, "the post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	cancel()
	<-first.Done()
}

// TestRecoveredTurnTakesOverAMessageThatWasNeverAStream: the row's ts is of a message the update path
// posted, so ending it as a stream is refused. That is no reason to leave it: it is updated.
func TestRecoveredTurnTakesOverAMessageThatWasNeverAStream(t *testing.T) {
	e := setup(t)
	e.postedByUpdate(t)
	again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
	again.Handle(text("a", "the whole answer"))
	again.Handle(agent.FinalEvent{})
	e.finish(t, again)
	th := e.fake.Thread("C1", e.root)
	if len(th) != 2 || th[1].Text != "the whole answer" || len(e.rec.sent("/chat.startStream")) != 0 || len(e.rec.sent("/chat.stopStream")) != 1 || e.updates() != 1 {
		t.Fatalf("thread %+v, %d starts, %d stops, %d updates; want one message with the whole text, never started, one stop refused, one update",
			th, len(e.rec.sent("/chat.startStream")), len(e.rec.sent("/chat.stopStream")), e.updates())
	}
	e.assertLastStopIsBare(t)
}

// TestNotStreamingIsMatchedByTypeNotByText: the refusal to stop a message that is not a stream is
// told by the code Slack gave, not by a message that happens to mention it.
func TestNotStreamingIsMatchedByTypeNotByText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		updates int // 1: the turn went on to update the message; 0: the stop failed and was retried until the deadline
	}{
		{"the sentinel, wrapped", fmt.Errorf("stop: %w", chat.ErrNotStreaming), 1},
		{"text that mentions it", errors.New("proxy said the message is not streaming"), 0},
		{"another error", fmt.Errorf("stop: %w", errors.New("channel_not_found")), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.postedByUpdate(t)
			e.cfg.Platform = stopFails{Platform: e.cfg.Platform, err: tc.err}
			e.live = newRenderer(t, e.cfg)
			again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
			again.Handle(text("a", "the whole answer"))
			again.Handle(agent.FinalEvent{})
			eventually(t, "the run to end", func() bool {
				select {
				case <-again.Done():
					return true
				default:
				}
				if e.clk.waiting() > 0 {
					e.clk.advance(30 * time.Second)
				}
				return false
			})
			if e.updates() != tc.updates {
				t.Fatalf("%d updates; want %d", e.updates(), tc.updates)
			}
		})
	}
}
