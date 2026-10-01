package live_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
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
	run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"}) // no tool lines in a stream
	e.clk.advance(throttle)
	eventually(t, "the append", func() bool { return e.reply().Text == "Hello wor" })
	if got := e.rec.sent("/chat.appendStream"); len(got) != 1 || got[0].Get("markdown_text") != "lo wor" {
		t.Fatalf("appends %v; want one, carrying only the new suffix", got)
	}
	if strings.Contains(string(e.reply().Blocks), "Calling") || len(e.rec.sent("/chat.update")) != 0 {
		t.Fatalf("a tool line or an update reached the stream: %+v", e.reply())
	}
	e.assertInert(t)
}

func TestStreamStopsWithTheTagAndTheFinalBlocks(t *testing.T) {
	e := setup(t)
	run := e.stream(t)
	run.Handle(text("a", "Hello"))
	eventually(t, "the stream to start", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
	run.Handle(text("a", "Hello world"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	if m.Streaming || m.Text != "Hello world" || !strings.Contains(string(m.Blocks), "Hello world") || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "turn-1/reply" {
		t.Fatalf("after the stop: %+v; want the whole text, the final blocks and the outbox tag", m)
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
	if m := e.reply(); m.Streaming || m.Text != "Goodbye, all" {
		t.Fatalf("after the stop: %+v; want the final text", m)
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
	<-run.Done()
	if m := e.reply(); m.Streaming || m.Text != "Hello world" || m.Metadata == nil {
		t.Fatalf("after the shutdown: %+v; want the stream stopped with the whole text and the tag", m)
	}
}

// TestRecoveredTurnWithItsTsStopsTheStreamAndUpdatesIt: a stream's ts was recorded, so the message
// exists and is courier's. A turn that continues it, after a restart or a lost connection, ends the
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
	row := store.OutboxEntry{ID: outbox.ID("turn-1", "reply"), EventID: "turn-1", Role: store.OutboxReply, Channel: "C1", ThreadTS: e.root, CreatedAt: e.clk.Now()}
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
