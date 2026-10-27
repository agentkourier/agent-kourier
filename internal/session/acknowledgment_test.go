package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A configured acknowledgment covers the whole accepted turn: it is added when intake accepts the
// message, stays on both the running and waiting messages, and is removed only when each turn ends.
func TestAcknowledgmentLifecycleUsesTheConfiguredReaction(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	sink := f.live()
	firstStarted, secondStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		if n == 0 {
			close(firstStarted)
		} else {
			close(secondStarted)
		}
		<-release
		return turn(h, n, a2a.TaskStateCompleted)
	}

	first := inbound("1")
	if res, err := f.submit(first); err != nil || res.Queued || res.Acknowledgment != "eyes" {
		t.Fatalf("first turn = %+v, %v; want an unqueued turn with its acknowledgment", res, err)
	}
	<-firstStarted
	second := inbound("2")
	if res, err := f.submit(second); err != nil || !res.Queued || res.Acknowledgment != "eyes" {
		t.Fatalf("second turn = %+v, %v; want a queued turn with its acknowledgment", res, err)
	}

	if got := sink.noticesOf(NoticeAcknowledged); !sameAcknowledgments(got, []Notice{
		{ID: first.MessageID + "/acknowledgment", Kind: NoticeAcknowledged, MessageRef: first.MessageRef, Reaction: "eyes"},
		{ID: second.MessageID + "/acknowledgment", Kind: NoticeAcknowledged, MessageRef: second.MessageRef, Reaction: "eyes"},
	}) {
		t.Fatalf("acknowledgment notices while the turns run = %+v", got)
	}
	if got := sink.noticesOf(NoticeUnacknowledged); len(got) != 0 {
		t.Fatalf("acknowledgment removals before a turn ends = %+v", got)
	}
	if got := sink.noticesOf(NoticeQueued); len(got) != 0 {
		t.Fatalf("legacy queued notices = %+v; configured acknowledgments own the lifecycle", got)
	}

	close(release)
	<-secondStarted
	waitFinal(t, sink)
	waitFinal(t, sink)
	f.retired(t)
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, []Notice{
		{ID: first.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: first.MessageRef, Reaction: "eyes"},
		{ID: second.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: second.MessageRef, Reaction: "eyes"},
	}) {
		t.Fatalf("acknowledgment removals after completion = %+v", got)
	}
}

// With Config.Unacknowledge set, a turn's removal goes through it, named by its source reply, in place of the sink, so
// that the wiring can settle the reply once the removal is queued. A failure is the hook's to report: the manager logs it
// and goes on, and the next turn's removal is still handed over.
func TestAcknowledgmentRemovalGoesThroughUnacknowledge(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	sink := f.live()
	type removal struct {
		source string
		n      Notice
	}
	var mu sync.Mutex
	var got []removal
	f.m.cfg.Unacknowledge = func(_ context.Context, _ Key, source string, n Notice) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, removal{source, n})
		if len(got) == 1 {
			return errors.New("outbox unavailable")
		}
		return nil
	}
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		return turn(h, n, a2a.TaskStateCompleted)
	}
	first, second := inbound("1"), inbound("2")
	for _, in := range []Inbound{first, second} {
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	f.retired(t)
	mu.Lock()
	defer mu.Unlock()
	want := []removal{
		{first.MessageID, Notice{ID: first.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: first.MessageRef, Reaction: "eyes"}},
		{second.MessageID, Notice{ID: second.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: second.MessageRef, Reaction: "eyes"}},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("removals handed to Unacknowledge = %+v; want %+v", got, want)
	}
	if n := sink.noticesOf(NoticeUnacknowledged); len(n) != 0 {
		t.Fatalf("removals on the sink = %+v; want none: Unacknowledge queues them", n)
	}
}

// Disabling the feature suppresses both the new acknowledgment and the legacy waiting marker. A
// binding reload therefore cannot leave a new reaction behind for a message it accepted.
func TestDisabledAcknowledgmentDoesNotMarkAcceptedMessages(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled = true, false
	sink := f.live()
	started, release := make(chan struct{}), make(chan struct{})
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		accepted(h, n)
		if n == 0 {
			close(started)
		}
		<-release
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-started
	if res, err := f.submit(inbound("2")); err != nil || !res.Queued || res.Acknowledgment != "" {
		t.Fatalf("disabled second turn = %+v, %v; want queued without an acknowledgment", res, err)
	}
	if got := sink.noticesOf(NoticeAcknowledged); len(got) != 0 {
		t.Fatalf("acknowledgment notices = %+v; want none", got)
	}
	if got := sink.noticesOf(NoticeQueued); len(got) != 0 {
		t.Fatalf("legacy queued notices = %+v; want none when disabled", got)
	}
	close(release)
	waitFinal(t, sink)
	waitFinal(t, sink)
	f.retired(t)
	if got := sink.noticesOf(NoticeUnacknowledged); len(got) != 0 {
		t.Fatalf("acknowledgment removals = %+v; want none", got)
	}
	if got := sink.noticesOf(NoticeDequeued); len(got) != 0 {
		t.Fatalf("legacy queued removals = %+v; want none", got)
	}
}

// The source message is settled when the agent posts a question. A typed answer is accepted as
// a new source and gets its own durable add/remove pair while it continues the paused task.
func TestAcknowledgmentIsClearedAtPauseAndRecreatedForTypedAnswer(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	answerStarted, releaseAnswer := make(chan struct{}), make(chan struct{})
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error {
		close(answerStarted)
		<-releaseAnswer
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	sink := f.live()
	first := inbound("1")
	if _, err := f.submit(first); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the paused message acknowledgment to be removed", func() bool {
		return len(sink.noticesOf(NoticeUnacknowledged)) == 1
	})
	eventually(t, "paused acknowledgment metadata to be cleared", func() bool {
		sess, err := f.session()
		return err == nil && sess.Turn.Acknowledgment == "" && sess.Turn.AcknowledgmentID == ""
	})

	answer := inbound("2")
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID, msg.TaskID, msg.ContextID = r.ID+"/answer", task.ID, task.ContextID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	if _, err := f.submit(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer was not sent")
	}
	if sess, err := f.session(); err != nil || sess.Turn.ID != answer.MessageID+"/answer" || sess.Turn.AcknowledgmentID != answer.MessageID || sess.Turn.MessageRef != answer.MessageRef || sess.Turn.Acknowledgment != "eyes" {
		t.Fatalf("answer session = %+v, %v; want the answer's durable acknowledgment metadata", sess, err)
	}
	close(releaseAnswer)
	waitFinal(t, sink)
	f.retired(t)
	wantRemovals := []Notice{
		{ID: first.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: first.MessageRef, Reaction: "eyes"},
		{ID: answer.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: answer.MessageRef, Reaction: "eyes"},
	}
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, wantRemovals) {
		t.Fatalf("pause and answer removals = %+v; want %+v", got, wantRemovals)
	}
}

// A typed answer's A2A message ID is interaction/answer, while its temporary reaction belongs to the
// incoming chat reply. Persisting both IDs keeps a restart from removing a different outbox row.
func TestTypedAnswerAcknowledgmentSurvivesCrashBeforeReplySettlement(t *testing.T) {
	f, sink := textPauseFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID, msg.TaskID, msg.ContextID = r.ID+"/answer", task.ID, task.ContextID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	answer := inbound("2")
	f.st.mu.Lock()
	f.st.fail["mark"] = errors.New("settlement interrupted")
	f.st.mu.Unlock()
	f.m.cfg.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	if _, err := f.submit(answer); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer turn to be durably paired with its source", func() bool {
		sess, err := f.session()
		return err == nil && sess.Turn.ID == answer.MessageID+"/answer" && sess.Turn.AcknowledgmentID == answer.MessageID
	})
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	f.st.mu.Lock()
	delete(f.st.fail, "mark")
	f.st.mu.Unlock()

	// Reopen the manager over the same store, as a process restart would. The task is now found ended;
	// recovery must use the incoming answer ID for the removal even though the recorded output has /answer.
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
	}
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		h(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateWorking})
		h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		return TextReplyConsumed, nil
	})
	cfg := f.m.cfg
	cfg.Sink = sink
	restartedManager, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.m = restartedManager
	f.m.wake = f.m.start
	if err := f.m.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the recovered answer task to clear", func() bool { return f.taskID() == "" })
	got := sink.noticesOf(NoticeUnacknowledged)
	var sourceRemoved bool
	for _, n := range got {
		if n.ID == answer.MessageID+"/acknowledgment" && n.MessageRef == answer.MessageRef && n.Reaction == "eyes" {
			sourceRemoved = true
		}
	}
	if !sourceRemoved {
		t.Fatalf("recovered answer acknowledgment removals = %+v; want source %q", got, answer.MessageID+"/acknowledgment")
	}
}

func TestConsumedTextReplyWithoutContinuationClearsItsAcknowledgment(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		return TextReplyConsumed, nil // rejected or stale answers are consumed without a continuation
	})
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the consumed answer acknowledgment to be removed", func() bool {
		for _, n := range sink.noticesOf(NoticeUnacknowledged) {
			if n.ID == inbound("2").MessageID+"/acknowledgment" {
				return true
			}
		}
		return false
	})
	if err := f.m.Cancel(t.Context(), key, "t1", "the question timed out"); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, []Notice{
		{ID: inbound("1").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "1", Reaction: "eyes"},
		{ID: inbound("2").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "2", Reaction: "eyes"},
	}) {
		t.Fatalf("consumed text reply removals = %+v; want one for the question and one for the consumed reply", got)
	}
}

func TestAnswerBusyReparkSettlesTheFailedAnswerAcknowledgment(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	var answers atomic.Int32
	firstAttempt := make(chan struct{})
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error {
		if answers.Add(1) == 1 {
			close(firstAttempt)
			return agent.ErrBusy
		}
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID, msg.TaskID, msg.ContextID = r.ID, task.ID, task.ContextID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstAttempt:
	case <-time.After(10 * time.Second):
		t.Fatal("the first answer was not attempted")
	}
	eventually(t, "the failed answer acknowledgment to be removed", func() bool {
		for _, n := range sink.noticesOf(NoticeUnacknowledged) {
			if n.ID == inbound("2").MessageID+"/acknowledgment" {
				return true
			}
		}
		return false
	})
	if _, err := f.submit(inbound("3")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	f.retired(t)
	want := []Notice{
		{ID: inbound("1").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "1", Reaction: "eyes"},
		{ID: inbound("2").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "2", Reaction: "eyes"},
		{ID: inbound("3").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "3", Reaction: "eyes"},
	}
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, want) {
		t.Fatalf("answer retry removals = %+v; want %+v", got, want)
	}
}

func TestFailedSettlementKeepsAcknowledgmentUntilDeliveryIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.agent.script = func(context.Context, int, agent.Handler) error { return agent.ErrForbidden }
	sink := f.live()
	f.st.fail["mark"] = errors.New("delivery write unavailable")
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	if got := sink.noticesOf(NoticeUnacknowledged); len(got) != 0 {
		t.Fatalf("acknowledgment removed after failed settlement = %+v; want it retained for retry", got)
	}
	f.m.start(key)
	f.retired(t)
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, []Notice{{
		ID: inbound("1").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "1", Reaction: "eyes",
	}}) {
		t.Fatalf("acknowledgment removal after retry = %+v; want one removal", got)
	}
}

func TestAcknowledgmentIsRemovedWhenRunningTurnIsStopped(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	sink := f.live()
	started, stopped := make(chan struct{}), make(chan struct{})
	f.agent.onCancel = func(a2a.TaskID) { close(stopped) }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			id := accepted(h, n)
			close(started)
			<-stopped
			h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}})
			return nil
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	first, second := inbound("1"), inbound("2")
	for _, in := range []Inbound{first, second} {
		if _, err := f.submit(in); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	if stoppedNow, err := f.m.Stop(t.Context(), key, "t1", "U1"); err != nil || !stoppedNow {
		t.Fatalf("Stop = %v, %v; want the running task stopped", stoppedNow, err)
	}
	waitFinal(t, sink)
	waitFinal(t, sink)
	f.retired(t)
	want := []Notice{
		{ID: first.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: first.MessageRef, Reaction: "eyes"},
		{ID: second.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: second.MessageRef, Reaction: "eyes"},
	}
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, want) {
		t.Fatalf("stopped turn removals = %+v; want %+v", got, want)
	}
}

// A recovered task has a synthetic queue row, but its acknowledgment belongs to the original
// message that began the task. Removal must use that durable source ID so the outbox row added at
// intake is the one it removes after a restart.
func TestRecoveredAcknowledgmentRemovalUsesOriginalMessageID(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.unacknowledge(&tstate{
		key:   key,
		r:     storeReply("task:t1", "1", "eyes"),
		ackID: inbound("1").MessageID,
	})
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, []Notice{{
		ID: inbound("1").MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: "1", Reaction: "eyes",
	}}) {
		t.Fatalf("recovered removal = %+v; want the original source ID", got)
	}
}

func TestRecoverRemovesPersistedAcknowledgmentAfterConfigurationDisablesIt(t *testing.T) {
	f, sink := restarted(t)
	f.ackConfigured, f.ackEnabled = true, false
	original := inbound("1")
	r := store.Reply{ID: original.MessageID, Key: key, UserID: original.UserID, MessageRef: original.MessageRef, Acknowledgment: "eyes", Text: original.Text, EnqueuedAt: t0}
	if _, err := f.st.EnqueueReply(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := f.st.MarkReplyDelivered(t.Context(), r.ID, t0); err != nil {
		t.Fatal(err)
	}
	recordedTurn(t, f, store.TaskTurn{ID: original.MessageID, User: original.UserID, MessageRef: original.MessageRef, Acknowledgment: "eyes"})
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the recovered task to be cleared", func() bool { return f.taskID() == "" })
	got := sink.noticesOf(NoticeUnacknowledged)
	want := []Notice{{ID: original.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: original.MessageRef, Reaction: "eyes"}}
	if !sameAcknowledgments(got, want) {
		t.Fatalf("recovered acknowledgment removals = %+v; want %+v", got, want)
	}
}

func TestRecoveredPauseClearsAcknowledgmentOwnership(t *testing.T) {
	f, sink := restarted(t)
	original := inbound("1")
	recordedTurn(t, f, store.TaskTurn{ID: original.MessageID + "/answer", User: original.UserID, MessageRef: original.MessageRef, Acknowledgment: "eyes", AcknowledgmentID: original.MessageID})
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}})
		return nil
	}
	f.recover(t)
	waitFinal(t, sink)
	eventually(t, "the recovered pause to clear its durable acknowledgment", func() bool {
		sess, err := f.session()
		return err == nil && sess.TaskID == "t1" && sess.Turn.Acknowledgment == "" && sess.Turn.AcknowledgmentID == ""
	})
	want := []Notice{{ID: original.MessageID + "/acknowledgment", Kind: NoticeUnacknowledged, MessageRef: original.MessageRef, Reaction: "eyes"}}
	if got := sink.noticesOf(NoticeUnacknowledged); !sameAcknowledgments(got, want) {
		t.Fatalf("recovered pause removals = %+v; want %+v", got, want)
	}
}

func storeReply(id, ref, reaction string) store.Reply {
	return store.Reply{ID: id, Key: key, MessageRef: ref, Acknowledgment: reaction}
}

func sameAcknowledgments(got, want []Notice) bool {
	return slices.Equal(got, want)
}
