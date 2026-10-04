package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
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

// noCalls is a platform that fails the test on any call that reaches the network: a Notice must not reach Slack.
type noCalls struct{ t *testing.T }

func (p noCalls) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	p.t.Error("a Slack post was made")
	return chat.Ref{}, nil
}

func (p noCalls) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	p.t.Error("a Slack lookup was made")
	return chat.Ref{}, false, nil
}

func (p noCalls) React(context.Context, chat.Ref, string) error {
	p.t.Error("a Slack reaction was made")
	return nil
}

func (p noCalls) Unreact(context.Context, chat.Ref, string) error {
	p.t.Error("a Slack unreaction was made")
	return nil
}

func (noCalls) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: "fake", Channel: channel, Thread: thread, ID: id}
}

func (noCalls) Fits(chat.Prompt) error { return nil }

func (noCalls) Classify(chat.Op, error) chat.Verdict { return chat.Verdict{Class: chat.Transient} }

func TestNoticeOnlyQueuesAndKeepsItsDetailLiteral(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	thread := thread
	thread.Thread = e.root
	for _, kind := range []session.NoticeKind{session.NoticeRetrying, session.NoticeStillBusy, session.NoticeNotReady, session.NoticeFailed, session.NoticeSessionGone, session.NoticeBindingGone, session.NoticeExpired, session.NoticeAnswerRejected, session.NoticeAnswerDropped, session.NoticeStopped, session.NoticeWedged, session.NoticeDuplicateRun, session.NoticeTaskFailed, session.NoticeTaskRejected, session.NoticeTaskCanceled, session.NoticeNoOutput} {
		n := session.Notice{ID: "task:t1/" + string(kind), Kind: kind, Detail: "<!channel> boom"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var out chat.Outgoing
		_ = json.Unmarshal([]byte(row.Payload), &out)
		if err != nil || row.Channel != "C1" || row.Thread != e.root || out.Message == nil || !strings.HasSuffix(out.Message.Fallback, " <!channel> boom") {
			t.Errorf("%s: row %+v, %v; want a queued row in the thread with the detail as it was, for the platform to make inert", kind, row, err)
		}
	}
	for _, kind := range []session.NoticeKind{session.NoticeQueued, session.NoticeDequeued} {
		sink.Notice(t.Context(), thread, session.Notice{ID: "m/queued", Kind: kind, MessageRef: "1.5"})
	}
	if _, err := e.store.GetOutbox(t.Context(), outbox.ID("m/queued", "notice")); err == nil {
		t.Error("the queued or dequeued notice became a message; it is a reaction")
	}
}

// TestStuckNoticesSayWhatTheTurnWaitsFor: a turn that has waited long says whether it is for a busy task or for a
// session not ready, in words that promise only what courier does: it keeps trying.
func TestStuckNoticesSayWhatTheTurnWaitsFor(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	thread := thread
	thread.Thread = e.root
	for kind, want := range map[session.NoticeKind]string{
		session.NoticeStillBusy: "The agent is still busy with an earlier task in this thread, so this message is waiting. I'll keep trying.",
		session.NoticeNotReady:  "The agent isn't ready to take this message yet, so it is waiting. I'll keep trying.",
	} {
		n := session.Notice{ID: "slack:ns/conn:C1:1.5/" + string(kind), Kind: kind, MessageRef: "1.5"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var out chat.Outgoing
		_ = json.Unmarshal([]byte(row.Payload), &out)
		if err != nil || out.Message == nil || out.Message.Fallback != want {
			t.Errorf("%s: row %+v, %v; want a notice in the thread that says %q", kind, row, err, want)
		}
	}
}

// TestDequeuedNoticeEnqueuesAnUnreactionOfTheQueuedReaction: the notice has the queued notice's ID, so its row is
// the removal of that reaction, with the same emoji, which the sink names in one place; asked twice, it is one row;
// and no call is made to Slack.
func TestDequeuedNoticeEnqueuesAnUnreactionOfTheQueuedReaction(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	n := session.Notice{ID: "slack:ns/conn:C1:1.5/queued", Kind: session.NoticeDequeued, MessageRef: "1.5"}
	for range 2 {
		sink.Notice(t.Context(), thread, n)
	}
	row, err := e.store.GetOutbox(t.Context(), outbox.UnreactionID(n.ID))
	if err != nil || row.Role != store.OutboxReaction || row.Channel != "C1" || row.MessageRef != "" || !strings.Contains(row.Payload, `"ts":"1.5"`) ||
		!strings.Contains(row.Payload, `"name":"`+live.DefaultReactions().Queued+`"`) || !strings.Contains(row.Payload, `"remove":true`) {
		t.Fatalf("row %+v, %v; want an unmade removal of the queued reaction on 1.5 in C1", row, err)
	}
	if rows, _ := e.store.ListUnposted(t.Context()); len(rows) != 1 {
		t.Fatalf("%d rows for one notice raised twice; want 1", len(rows))
	}
}

// TestQueuedReactionIsTakenOffTheMessage runs the rows to Slack, in both orders the notices can come: the
// hourglass is on while the reply waits, and off once it was sent; and a removal that comes first leaves nothing.
func TestQueuedReactionIsTakenOffTheMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []session.NoticeKind // in the order the sink gets them
		mid   int                  // the notices after which the outbox drains once, to see the hourglass
	}{
		{"queued, then sent", []session.NoticeKind{session.NoticeQueued, session.NoticeDequeued}, 1},
		{"sent, then queued", []session.NoticeKind{session.NoticeDequeued, session.NoticeQueued}, 0},
		{"queued and sent before it was drained", []session.NoticeKind{session.NoticeQueued, session.NoticeDequeued}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			sink := e.sink(t, e.ob)
			key := thread
			key.Thread = e.root
			for i, kind := range tc.kinds {
				sink.Notice(t.Context(), key, session.Notice{ID: "slack:ns/conn:C1:" + e.root + "/queued", Kind: kind, MessageRef: e.root})
				if i+1 == tc.mid {
					if err := e.ob.Reconcile(t.Context()); err != nil {
						t.Fatalf("Reconcile = %v", err)
					}
					if got := e.fake.Reactions("C1", e.root); len(got) != 1 || got[0].Name != live.DefaultReactions().Queued {
						t.Fatalf("Reactions while the reply waits = %+v; want the hourglass", got)
					}
				}
			}
			if err := e.ob.Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile = %v", err)
			}
			if got := e.fake.Reactions("C1", e.root); len(got) != 0 {
				t.Fatalf("Reactions after the turn was sent = %+v; want none", got)
			}
		})
	}
}

// TestQueuedNoticeEnqueuesAReactionAndSendsNothing: the notice names the message, so the row is a
// reaction on it, in the thread's channel, under the notice's ID; the same notice twice is one row;
// and no call is made to Slack, which is waiting for the acknowledgement this runs inside.
func TestQueuedNoticeEnqueuesAReactionAndSendsNothing(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	n := session.Notice{ID: "slack:ns/conn:C1:1.5/queued", Kind: session.NoticeQueued, MessageRef: "1.5"}
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

// TestQueuedReactionIsMadeOnTheMessage runs the row to Slack: the reply that waits is marked.
func TestQueuedReactionIsMadeOnTheMessage(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, e.ob)
	key := thread
	key.Thread = e.root
	n := session.Notice{ID: "slack:ns/conn:C1:" + e.root + "/queued", Kind: session.NoticeQueued, MessageRef: e.root}
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
	if rows, _ := e.store.ListUnposted(t.Context()); len(rows) != 0 || !strings.Contains(logs.String(), "no valid row was dropped") {
		t.Fatalf("%d rows, log %q; want none, and the failure logged", len(rows), logs.String())
	}
}

func TestNoticeOfAnUnknownKindIsLogged(t *testing.T) {
	e := setup(t)
	logs := &strings.Builder{}
	sink := live.NewSink(e.live, e.ob, slog.New(slog.NewTextHandler(logs, nil)))
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/odd", Kind: "never_heard_of_it"})
	sink.Notice(t.Context(), thread, session.Notice{ID: "m/queued", Kind: session.NoticeQueued, MessageRef: "1.5"})
	if got := logs.String(); !strings.Contains(got, "never_heard_of_it") || strings.Contains(got, "queued") {
		t.Fatalf("log %q; want the unknown kind named, and the queued one, a reaction, left out", got)
	}
}

// TestSinkPassesRecoveredOn: a turn found again after a restart updates the message it posted
// before, which Recovered tells the renderer to look for.
func TestSinkPassesRecoveredOn(t *testing.T) {
	e := setup(t)
	if _, err := e.ob.Send(t.Context(), outbox.Post{Source: "turn-1", Role: "reply", Channel: "C1", Thread: e.root, Out: chat.Say(chat.Message{Fallback: "before the crash"})}); err != nil {
		t.Fatal(err)
	}
	key := thread
	key.Thread = e.root
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
	thread.Thread = e.root
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

func (b *blocker) Send(ctx context.Context, _ outbox.Post) (chat.Ref, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return chat.Ref{}, ctx.Err()
}

// TestTurnHandlerDoesNotWaitOnSlack: with Slack not answering, the stream reader's handler still returns.
func TestTurnHandlerDoesNotWaitOnSlack(t *testing.T) {
	e := setup(t)
	slow := &blocker{release: make(chan struct{})}
	defer close(slow.release) // Slack answers in the end, and the run's write returns
	r := live.New(live.Config{Sender: slow, Platform: slack.NewPlatform(nil, "ns/conn"), Throttle: throttle, Now: e.clk.Now, After: e.clk.After, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
			key.Thread = e.root
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
	key.Thread = e.root
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
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	thread := thread
	thread.Thread = e.root
	for kind, want := range map[session.NoticeKind]string{
		session.NoticeExpired:     "This conversation has expired, so I can't continue it. Mention me in the channel to start a new one.",
		session.NoticeBindingGone: "This thread's agent is no longer set up, so I can't pass your messages on.",
	} {
		n := session.Notice{ID: "m1/" + string(kind), Kind: kind, MessageRef: "5.5"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var out chat.Outgoing
		_ = json.Unmarshal([]byte(row.Payload), &out)
		if err != nil || out.Message == nil || out.Message.Fallback != want {
			t.Errorf("%s: message %+v, %v; want %q", kind, out.Message, err, want)
		}
	}
}

// The notices of a turn that ended without an answer say what happened in courier's own fixed words, and
// name no cause: the agent's status text is untrusted and not courier's to repeat (spec 8.4, R2).
func TestNoticesOfATaskThatEndedWithoutAnAnswerAreFixedText(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	thread := thread
	thread.Thread = e.root
	for kind, want := range map[session.NoticeKind]string{
		session.NoticeTaskFailed:   "The agent's task failed, so there is no answer. Its own logs say why.",
		session.NoticeTaskRejected: "The agent rejected this request, so there is no answer.",
		session.NoticeTaskCanceled: "The agent's task was canceled before it finished.",
		session.NoticeNoOutput:     "The agent finished without any output.",
	} {
		n := session.Notice{ID: "task:t1/" + string(kind), Kind: kind, MessageRef: "5.5"}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var out chat.Outgoing
		_ = json.Unmarshal([]byte(row.Payload), &out)
		if err != nil || out.Message == nil || out.Message.Fallback != want {
			t.Errorf("%s: message %+v, %v; want %q", kind, out.Message, err, want)
		}
	}
}

// The notes a trigger's thread gets for a repeat and for a close are courier's own words and the time it
// fills in: nothing of the message that fired them is in them.
func TestNoticesOfATriggerThreadAreFixedTextAndATime(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, outbox.New(outbox.Config{Store: e.store, Platform: noCalls{t}}))
	thread := thread
	thread.Thread = e.root
	for kind, want := range map[session.NoticeKind]string{
		session.NoticeTriggerRepeat: "Fired again at 2026-10-04 12:30 UTC.",
		session.NoticeTriggerClosed: "Resolved at 2026-10-04 12:30 UTC.",
	} {
		n := session.Notice{ID: "slack:ns/conn:C1:5.5/" + string(kind), Kind: kind, MessageRef: "5.5", Detail: "2026-10-04 12:30 UTC."}
		sink.Notice(t.Context(), thread, n)
		row, err := e.store.GetOutbox(t.Context(), outbox.ID(n.ID, "notice"))
		var out chat.Outgoing
		_ = json.Unmarshal([]byte(row.Payload), &out)
		if err != nil || row.Thread != e.root || out.Message == nil || out.Message.Fallback != want {
			t.Errorf("%s: row %+v, message %+v, %v; want a note in the thread that says %q", kind, row, out.Message, err, want)
		}
	}
}

// flaky is an Enqueuer whose first fails calls fail with err, and that then writes through to q.
type flaky struct {
	q     live.Enqueuer
	err   error
	fails int
}

func (f *flaky) Enqueue(ctx context.Context, p outbox.Post) (string, error) {
	if f.fails > 0 {
		f.fails--
		return "", f.err
	}
	return f.q.Enqueue(ctx, p)
}

func (f *flaky) EnqueueReaction(ctx context.Context, r outbox.Reaction) (string, error) {
	if f.fails > 0 {
		f.fails--
		return "", f.err
	}
	return f.q.EnqueueReaction(ctx, r)
}

func (f *flaky) EnqueueUnreaction(ctx context.Context, r outbox.Reaction) (string, error) {
	if f.fails > 0 {
		f.fails--
		return "", f.err
	}
	return f.q.EnqueueUnreaction(ctx, r)
}

// TestQueueNoticeReturnsWhatKeepsANoticeFromBeingWritten: a notice that was not written is the
// caller's to retry, so QueueNotice returns the error of the write, for a message and for a
// reaction. What no retry can mend is not an error but a log: a notice of no wording, and one
// that is no valid row. Notice, the Sink's own, drops the error after logging it.
func TestQueueNoticeReturnsWhatKeepsANoticeFromBeingWritten(t *testing.T) {
	e := setup(t)
	boom := errors.New("disk full")
	logs := &strings.Builder{}
	sink := live.NewSink(e.live, &flaky{q: e.ob, err: boom, fails: 100}, slog.New(slog.NewTextHandler(logs, nil)))
	for _, n := range []session.Notice{
		{ID: "m/failed", Kind: session.NoticeFailed},
		{ID: "m/queued", Kind: session.NoticeQueued, MessageRef: "1.5"},
		{ID: "m/queued", Kind: session.NoticeDequeued, MessageRef: "1.5"},
	} {
		if err := sink.QueueNotice(t.Context(), thread, n); !errors.Is(err, boom) {
			t.Errorf("QueueNotice(%s) = %v; want the error of the write", n.Kind, err)
		}
		logs.Reset()
		sink.Notice(t.Context(), thread, n) // the plain one cannot return it
		if !strings.Contains(logs.String(), "disk full") {
			t.Errorf("Notice(%s) logged %q; want the failure logged", n.Kind, logs.String())
		}
	}
	good := e.sink(t, e.ob)
	for name, n := range map[string]session.Notice{
		"no wording":                    {ID: "m/odd", Kind: "never_heard_of_it"},
		"a reaction with no message":    {ID: "m/queued", Kind: session.NoticeQueued},
		"an unreaction with no message": {ID: "m/queued", Kind: session.NoticeDequeued},
	} {
		if err := good.QueueNotice(t.Context(), thread, n); err != nil {
			t.Errorf("QueueNotice(%s) = %v; want nil: no retry mends it", name, err)
		}
	}
	if err := good.QueueNotice(t.Context(), thread, session.Notice{ID: "m/failed", Kind: session.NoticeFailed}); err != nil {
		t.Errorf("QueueNotice of a good notice = %v", err)
	}
}

// TestAFailedNoticeOfTheIntakeIsOneRowWhenRedelivered: the whole path. A reply in an expired thread
// raises a notice, its write fails once, and Slack redelivers the event: the notice is written then,
// and a further redelivery asks for nothing. The row's ID comes from the message, so there is one.
func TestAFailedNoticeOfTheIntakeIsOneRowWhenRedelivered(t *testing.T) {
	e := setup(t)
	flake := &flaky{q: e.ob, err: errors.New("disk full"), fails: 1}
	sink := e.sink(t, flake)
	submits := 0
	in := chat.NewIntake(chat.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "ns", Name: "conn"},
		Snapshot:   func() *config.Snapshot { return nil },
		Submitter: submitFunc(func(session.Inbound) (session.Result, error) {
			submits++
			return session.Result{Disposition: session.Expired}, nil
		}),
		Notice: sink.QueueNotice,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m := chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/conn", Channel: "C1", ID: "5.5", Thread: e.root}, User: "U1", Text: "hi"}
	if err := in.Message(t.Context(), m); err == nil {
		t.Fatal("Message with a notice that failed to write = nil; the event would be acknowledged and lost")
	}
	for i := range 2 { // the redelivery, and one more
		if err := in.Message(t.Context(), m); err != nil {
			t.Fatalf("redelivery %d = %v", i+1, err)
		}
	}
	id := outbox.ID(chat.MessageID(chat.Ref{Platform: "slack", Conn: "ns/conn", Channel: "C1", ID: "5.5"})+"/expired", store.OutboxNotice)
	rows, err := e.store.ListUnposted(t.Context())
	if err != nil || len(rows) != 1 || rows[0].ID != id || submits != 2 {
		t.Fatalf("rows %+v, %v after %d submits; want the one notice %s, written on the second delivery and not the third", rows, err, submits, id)
	}
}

type submitFunc func(session.Inbound) (session.Result, error)

func (f submitFunc) Submit(_ context.Context, in session.Inbound) (session.Result, error) {
	return f(in)
}
