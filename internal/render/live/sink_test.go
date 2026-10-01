package live_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

var thread = session.Key{Connection: "ns/conn", Channel: "C1"}

func (e *env) sink(t *testing.T, q live.Enqueuer) *live.Sink {
	t.Helper()
	return live.NewSink(e.live, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// noCalls is a Poster that fails the test on any call: a Notice must not reach Slack.
type noCalls struct{ t *testing.T }

func (p noCalls) Post(context.Context, string, string, slack.Content, string) (string, error) {
	p.t.Error("a Slack post was made")
	return "", nil
}

func (p noCalls) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	p.t.Error("a Slack lookup was made")
	return "", false, nil
}

func TestNoticeOnlyQueuesAndEscapesItsDetail(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Poster: noCalls{t}}))
	thread := thread
	thread.ThreadTS = e.root
	for _, kind := range []session.NoticeKind{session.NoticeRetrying, session.NoticeFailed, session.NoticeSessionGone, session.NoticeBindingGone, session.NoticeExpired, session.NoticeAnswerRejected, session.NoticeAnswerDropped, session.NoticeStopped, session.NoticeWedged, session.NoticeDuplicateRun} {
		n := session.Notice{ID: "task:t1/" + string(kind), Kind: kind, Detail: "<!channel> boom"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var content slack.Content
		_ = json.Unmarshal([]byte(row.Payload), &content)
		if err != nil || row.Channel != "C1" || row.ThreadTS != e.root || !strings.HasSuffix(content.Text, " &lt;!channel&gt; boom") || strings.Contains(content.Text, "<!channel>") {
			t.Errorf("%s: row %+v, %v; want a queued row in the thread with the detail escaped", kind, row, err)
		}
	}
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/queued", Kind: session.NoticeQueued, MessageTS: "1.5"})
	if _, err := e.store.GetOutbox(t.Context(), outbox.ID("m/queued", "notice")); err == nil {
		t.Error("the queued notice became a message; it is a reaction")
	}
}

// TestQueuedNoticeEnqueuesAReactionAndSendsNothing: the notice names the message, so the row is a
// reaction on it, in the thread's channel, under the notice's ID; the same notice twice is one row;
// and no call is made to Slack, which is waiting for the acknowledgement this runs inside.
func TestQueuedNoticeEnqueuesAReactionAndSendsNothing(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Poster: noCalls{t}, Reactor: noReactions{t}}))
	n := session.Notice{ID: "slack:ns/conn:C1:1.5/queued", Kind: session.NoticeQueued, MessageTS: "1.5"}
	for range 2 {
		sink.Notice(t.Context(), thread, n)
	}
	id := n.ID + "/reaction"
	row, err := e.store.GetOutbox(t.Context(), id)
	if err != nil || row.Role != store.OutboxReaction || row.Channel != "C1" || row.MessageRef != "" || !strings.Contains(row.Payload, `"ts":"1.5"`) || !strings.Contains(row.Payload, `"name":"hourglass_flowing_sand"`) {
		t.Fatalf("row %+v, %v; want an unmade reaction on 1.5 in C1", row, err)
	}
	if rows, _ := e.store.ListUnposted(t.Context()); len(rows) != 1 {
		t.Fatalf("%d rows for one notice raised twice; want 1", len(rows))
	}
}

// noReactions is a Reactor that fails the test on any call.
type noReactions struct{ t *testing.T }

func (r noReactions) React(context.Context, string, string, string) error {
	r.t.Error("a Slack reaction was made")
	return nil
}

// TestQueuedReactionIsMadeOnTheMessage runs the row to Slack: the reply that waits is marked.
func TestQueuedReactionIsMadeOnTheMessage(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, e.ob)
	key := thread
	key.ThreadTS = e.root
	n := session.Notice{ID: "slack:ns/conn:C1:" + e.root + "/queued", Kind: session.NoticeQueued, MessageTS: e.root}
	sink.Notice(t.Context(), key, n)
	if err := e.ob.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if got := e.fake.Reactions("C1", e.root); len(got) != 1 || got[0].Name != "hourglass_flowing_sand" {
		t.Fatalf("Reactions = %+v; want the hourglass on the message", got)
	}
}

// TestQueuedNoticeWithoutAMessageIsLogged: a notice that names no message cannot be a reaction, and
// is not silently lost.
func TestQueuedNoticeWithoutAMessageIsLogged(t *testing.T) {
	e := setup(t)
	logs := &strings.Builder{}
	sink := live.NewSink(e.live, e.ob, slog.New(slog.NewTextHandler(logs, nil)))
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/queued", Kind: session.NoticeQueued})
	if rows, _ := e.store.ListUnposted(t.Context()); len(rows) != 0 || !strings.Contains(logs.String(), "reaction was not queued") {
		t.Fatalf("%d rows, log %q; want none, and the failure logged", len(rows), logs.String())
	}
}

func TestNoticeOfAnUnknownKindIsLogged(t *testing.T) {
	e := setup(t)
	logs := &strings.Builder{}
	sink := live.NewSink(e.live, e.ob, slog.New(slog.NewTextHandler(logs, nil)))
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/odd", Kind: "never_heard_of_it"})
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/queued", Kind: session.NoticeQueued, MessageTS: "1.5"})
	if got := logs.String(); !strings.Contains(got, "never_heard_of_it") || strings.Contains(got, "queued") {
		t.Fatalf("log %q; want the unknown kind named, and the queued one, a reaction, left out", got)
	}
}

// TestSinkPassesRecoveredOn: a turn found again after a restart updates the message it posted
// before, which Recovered tells the renderer to look for.
func TestSinkPassesRecoveredOn(t *testing.T) {
	e := setup(t)
	if _, err := e.ob.Send(t.Context(), outbox.Post{Source: "turn-1", Role: "reply", Channel: "C1", ThreadTS: e.root, Content: slack.Content{Text: "before the crash"}}); err != nil {
		t.Fatal(err)
	}
	key := thread
	key.ThreadTS = e.root
	handle := e.sink(t, e.ob).Turn(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: session.SystemUser, Recovered: true})
	handle(text("a", "the whole answer"))
	handle(agent.FinalEvent{})
	eventually(t, "the message to say the whole answer", func() bool { e.clk.advance(throttle); return e.reply().Text == "the whole answer" })
	if posts := len(e.rec.sent("/chat.postMessage")); posts != 1 {
		t.Fatalf("%d posts; want the earlier one only, updated", posts)
	}
}

func TestTheSameNoticeIsOneRowAndOnePost(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, e.ob)
	thread := thread
	thread.ThreadTS = e.root
	n := session.Notice{ID: "task:t1/stopped", Kind: session.NoticeStopped}
	sink.Notice(t.Context(), thread, n)
	sink.Notice(t.Context(), thread, n) // the mention pair, or a repeated stop
	if err := e.ob.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if th := e.fake.Thread("C1", e.root); len(th) != 2 || th[1].Text != "The task was stopped." || th[1].Metadata.EventPayload["outbox_id"] != "task:t1/stopped/notice" {
		t.Fatalf("thread %+v; want one notice, tagged with its outbox ID", th)
	}
}

// blocker is a Sender that waits for release, as Slack may.
type blocker struct{ release chan struct{} }

func (b *blocker) Send(ctx context.Context, _ outbox.Post) (string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return "", ctx.Err()
}

// TestTurnHandlerDoesNotWaitOnSlack: with Slack not answering, the stream reader's handler still returns.
func TestTurnHandlerDoesNotWaitOnSlack(t *testing.T) {
	e := setup(t)
	slow := &blocker{release: make(chan struct{})}
	defer close(slow.release) // Slack answers in the end, and the run's write returns
	r := live.New(live.Config{Sender: slow, Throttle: throttle, Now: e.clk.Now, After: e.clk.After, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handle := live.NewSink(r, e.ob, nil).Turn(ctx, session.TurnRef{Key: thread, MessageID: "m1", UserID: session.SystemUser})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			handle(text("a", strings.Repeat("x", i+1)))
		}
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("the handler waited on Slack")
	}
}

// TestModeReachesTheRenderer: who asked and the Binding's output decide the path: a stream for a
// person in live mode, a post for an alert, and one post at the end in final mode.
func TestModeReachesTheRenderer(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		user                        string
		output                      v1alpha1.OutputMode
		wantStreams, wantFirstPosts int
	}{
		{"a person, live", "U1", v1alpha1.OutputLive, 1, 0},
		{"an alert, live", session.SystemUser, v1alpha1.OutputLive, 0, 1},
		{"a person, final", "U1", v1alpha1.OutputFinal, 0, 1},
		{"no output set means live", "U1", "", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			key := thread
			key.ThreadTS = e.root
			handle := e.sink(t, e.ob).Turn(ctx, session.TurnRef{Key: key, MessageID: "turn-1", UserID: tc.user, Output: tc.output})
			handle(text("a", "hello"))
			handle(agent.FinalEvent{})
			eventually(t, "the message", func() bool { return len(e.rec.sent("/chat.startStream"))+len(e.rec.sent("/chat.postMessage")) >= 1 })
			eventually(t, "the turn to end", func() bool { e.clk.advance(throttle); return e.reply().Text == "hello" && !e.reply().Streaming })
			eventually(t, "the run to have nothing left to wait for", func() bool { e.clk.advance(throttle); return e.clk.waiting() == 0 })
			if streams, posts := len(e.rec.sent("/chat.startStream")), len(e.rec.sent("/chat.postMessage")); streams != tc.wantStreams || posts != tc.wantFirstPosts {
				t.Fatalf("%d streams and %d posts; want %d and %d", streams, posts, tc.wantStreams, tc.wantFirstPosts)
			}
		})
	}
}

// TestTurnWithoutFinalFreesItsGoroutine: the session cancels a turn's context when its send returns,
// and a turn that never got a FinalEvent ends with it (goleak checks, in setup's cleanup).
func TestTurnWithoutFinalFreesItsGoroutine(t *testing.T) {
	e := setup(t)
	ctx, endTurn := context.WithCancel(t.Context())
	key := thread
	key.ThreadTS = e.root
	handle := e.sink(t, e.ob).Turn(ctx, session.TurnRef{Key: key, MessageID: "turn-1", UserID: session.SystemUser})
	handle(text("a", "partial"))
	eventually(t, "the post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	handle(text("a", "partial, more"))
	eventually(t, "the window", func() bool { return e.clk.waiting() == 1 })
	endTurn()
}

// The wording of the two notices for a thread courier cannot continue is courier's own and fixed:
// what the person wrote is nowhere in it.
func TestNoticesOfAThreadThatCannotContinueAreFixedText(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Poster: noCalls{t}}))
	thread := thread
	thread.ThreadTS = e.root
	for kind, want := range map[session.NoticeKind]string{
		session.NoticeExpired:     "This conversation has expired, so I can't continue it. Mention me in the channel to start a new one.",
		session.NoticeBindingGone: "This thread's agent is no longer set up, so I can't pass your messages on.",
	} {
		n := session.Notice{ID: "m1/" + string(kind), Kind: kind, MessageTS: "5.5"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var content slack.Content
		_ = json.Unmarshal([]byte(row.Payload), &content)
		if err != nil || content.Text != want {
			t.Errorf("%s: text %q, %v; want %q", kind, content.Text, err, want)
		}
	}
}
