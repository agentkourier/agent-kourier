package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func textPauseFixture(t *testing.T) (*fixture, *fakeSink) {
	t.Helper()
	return textPauseFixtureIn(t, "")
}

// textPauseFixtureIn is textPauseFixture for a Binding whose chat.threadReplies is mode. The first turn creates
// the session, so it is the same in every mode.
func textPauseFixtureIn(t *testing.T, mode v1alpha1.ThreadReplies) (*fixture, *fakeSink) {
	t.Helper()
	f := newFixture(t)
	f.textReplies = true
	f.threadReplies = mode
	f.m.cfg.RetryBase = time.Millisecond
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error {
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	return f, sink
}

func TestTextReplyCancellationEndsRetryAndDrainsQueue(t *testing.T) {
	for _, result := range []error{nil, errors.New("unusable question")} {
		t.Run("decision_"+fmtError(result), func(t *testing.T) {
			f, _ := textPauseFixture(t)
			var calls atomic.Int32
			f.setTextReply(func(ctx context.Context, k Key, _ store.Reply, task a2a.Task) (TextReplyOutcome, error) {
				calls.Add(1)
				if err := f.m.Cancel(ctx, k, task.ID, "refused question"); err != nil {
					return TextReplyWait, err
				}
				return TextReplyWait, result
			})
			if _, err := f.submit(inbound("2")); err != nil {
				t.Fatal(err)
			}
			f.retired(t)
			f.agent.mu.Lock()
			defer f.agent.mu.Unlock()
			if calls.Load() != 1 || len(f.agent.msgs) != 2 || len(f.agent.answers) != 0 {
				t.Fatalf("decisions=%d turns=%d answers=%d", calls.Load(), len(f.agent.msgs), len(f.agent.answers))
			}
		})
	}
}

func fmtError(err error) string {
	if err == nil {
		return "waiting"
	}
	return "error"
}

func TestTextReplyEndedTaskDrainsQueueWithoutDecision(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCanceled, a2a.TaskStateCompleted, a2a.TaskStateFailed} {
		t.Run(string(state), func(t *testing.T) {
			f, _ := textPauseFixture(t)
			f.agent.mu.Lock()
			f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
				return &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: state}}, nil
			}
			f.agent.mu.Unlock()
			var calls atomic.Int32
			f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
				calls.Add(1)
				return TextReplyWait, nil
			})
			if _, err := f.submit(inbound("2")); err != nil {
				t.Fatal(err)
			}
			f.retired(t)
			if calls.Load() != 0 {
				t.Fatal("ended task received a text decision")
			}
		})
	}
}

func TestTextReplyAuthRequiredWaitsAndCanBeStopped(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	waiting := make(chan struct{}, 1)
	retry := make(chan time.Time)
	f.m.cfg.After = func(time.Duration) <-chan time.Time {
		waiting <- struct{}{}
		return retry
	}
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return turn(h, n, a2a.TaskStateAuthRequired)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	var reads, decisions atomic.Int32
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		reads.Add(1)
		return &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateAuthRequired}}, nil
	}
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		return turn(h, 0, a2a.TaskStateAuthRequired)
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		decisions.Add(1)
		return TextReplyWait, nil
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
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("auth-required task never waited for a retry")
	}
	f.agent.mu.Lock()
	follows := len(f.agent.follows)
	f.agent.mu.Unlock()
	if reads.Load() != 1 || decisions.Load() != 0 || follows != 0 {
		t.Fatalf("reads=%d decisions=%d follows=%d", reads.Load(), decisions.Load(), follows)
	}
	if stopped, err := f.m.Stop(t.Context(), key, "t1", "U1"); !stopped || err != nil {
		t.Fatalf("stop during auth wait = %v, %v", stopped, err)
	}
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.msgs) != 2 || len(f.agent.answers) != 0 {
		t.Fatal("stop did not release the queued ordinary turn")
	}
}

func TestTextReplyDeliveryWriteRetriesBeforeSendingAnswer(t *testing.T) {
	f, _ := textPauseFixture(t)
	f.st.mu.Lock()
	f.st.fail["mark"] = errors.New("delivery write failed")
	f.st.mu.Unlock()
	waiting := make(chan struct{}, 1)
	retry := make(chan time.Time, 1)
	f.m.cfg.After = func(time.Duration) <-chan time.Time {
		waiting <- struct{}{}
		return retry
	}
	var decisions atomic.Int32
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		decisions.Add(1)
		msg := answerMsg(r.ID)
		msg.TaskID = task.ID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery write was not retried")
	}
	f.agent.mu.Lock()
	answers := len(f.agent.answers)
	f.agent.mu.Unlock()
	if answers != 0 {
		t.Fatal("answer sent before its reply delivery was recorded")
	}
	if r, err := f.st.NextReply(t.Context(), key); err != nil || r.ID != inbound("2").MessageID {
		t.Fatalf("reply lost during write failure: %+v, %v", r, err)
	}
	retry <- t0
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if decisions.Load() != 1 || len(f.agent.answers) != 1 || len(f.agent.msgs) != 1 {
		t.Fatalf("decisions=%d answers=%d turns=%d", decisions.Load(), len(f.agent.answers), len(f.agent.msgs))
	}
}

func TestTextReplySkipsEarlierTurnAndAnswersLaterReply(t *testing.T) {
	f, _ := textPauseFixture(t)
	var skipped atomic.Int32
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		if r.ID == inbound("2").MessageID {
			skipped.Add(1)
			return TextReplyNotAnswer, nil
		}
		msg := answerMsg(r.ID)
		msg.TaskID = task.ID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	for _, id := range []string{"2", "3"} {
		if _, err := f.submit(inbound(id)); err != nil {
			t.Fatal(err)
		}
	}
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if skipped.Load() != 1 || len(f.agent.answers) != 1 || f.agent.answers[0].ID != inbound("3").MessageID || len(f.agent.msgs) != 2 || f.agent.msgs[1].ID != inbound("2").MessageID {
		t.Fatalf("skipped=%d answers=%v turns=%v", skipped.Load(), f.agent.answers, f.agent.msgs)
	}
}

func TestTextReplyRecoveredClaimNeverBecomesOrdinaryTurn(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	r := store.Reply{ID: inbound("2").MessageID, Key: key, UserID: "U1", MessageRef: "2", Text: "answer", EnqueuedAt: t0}
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.EnqueueReply(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	claims := f.st.Store.(interface {
		ClaimReplyInteraction(context.Context, string, string) error
	})
	if err := claims.ClaimReplyInteraction(t.Context(), r.ID, "delivered-question"); err != nil {
		t.Fatal(err)
	}
	var reconciled atomic.Int32
	f.setTextReply(func(_ context.Context, _ Key, got store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		if got.InteractionID != "delivered-question" || task.ID != "" {
			t.Error("recovery did not reconcile the original claim")
		}
		reconciled.Add(1)
		return TextReplyConsumed, nil // the persisted interaction already records this answer as delivered
	})
	f.live()
	f.m.start(key)
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if reconciled.Load() != 1 || len(f.agent.msgs) != 0 || len(f.agent.answers) != 0 {
		t.Fatalf("reconciled=%d turns=%d answers=%d", reconciled.Load(), len(f.agent.msgs), len(f.agent.answers))
	}
	if _, err := f.st.NextReply(t.Context(), key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recovered answer remains queued: %v", err)
	}
}

func TestQueuedTextReplyAnswersPauseAndLaterRepliesRemainTurns(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	var decided atomic.Int32
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return task, nil }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return turn(h, n, a2a.TaskStateInputRequired)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	hold := make(chan struct{})
	f.agent.answer = func(msg *a2a.Message, h agent.Handler) error {
		h(agent.StateEvent{TaskID: msg.TaskID, ContextID: msg.ContextID, State: a2a.TaskStateWorking})
		<-hold
		h(agent.FinalEvent{Task: &a2a.Task{ID: msg.TaskID, ContextID: msg.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		decided.Add(1)
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID = r.ID
		msg.TaskID = task.ID
		msg.ContextID = task.ContextID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "text answer", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.answers) == 1 })
	if _, err := f.submit(inbound("3")); err != nil {
		t.Fatal(err)
	}
	close(hold)
	eventually(t, "second normal turn", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.msgs) == 2 })
	f.retired(t)
	if decided.Load() != 1 {
		t.Fatalf("text decisions=%d", decided.Load())
	}
	if _, err := f.st.NextReply(t.Context(), key); err == nil {
		t.Fatal("reply queue did not drain")
	}
}

func TestTextReplyWaitsForPromptWithoutLosingQueue(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.m.cfg.RetryBase = time.Millisecond
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return task, nil }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateInputRequired) }
	var tries atomic.Int32
	f.agent.answer = func(msg *a2a.Message, h agent.Handler) error {
		h(agent.StateEvent{TaskID: msg.TaskID, ContextID: msg.ContextID, State: a2a.TaskStateWorking})
		h(agent.FinalEvent{Task: &a2a.Task{ID: msg.TaskID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		if tries.Add(1) < 3 {
			return TextReplyWait, nil
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID = r.ID
		msg.TaskID = task.ID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "retried answer", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.answers) == 1 })
	f.retired(t)
	if tries.Load() != 3 {
		t.Fatalf("tries=%d", tries.Load())
	}
}

func TestTextReplyDrainsRejectedRepliesWhenWakesCoalesce(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.m.cfg.RetryBase = time.Millisecond
	task := &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return task, nil }
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error { return turn(h, n, a2a.TaskStateInputRequired) }
	var ready atomic.Bool
	var consumed atomic.Int32
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		if !ready.Load() {
			return TextReplyWait, nil
		}
		consumed.Add(1)
		return TextReplyConsumed, nil // a denied answer is consumed without resuming the task
	})
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	for _, id := range []string{"2", "3", "4"} {
		if _, err := f.submit(inbound(id)); err != nil {
			t.Fatal(err)
		}
	}
	ready.Store(true)
	eventually(t, "rejected queue drained", func() bool {
		_, err := f.st.NextReply(t.Context(), key)
		return consumed.Load() == 3 && err != nil
	})
	eventually(t, "consumed replies released", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return len(f.m.fresh) == 0
	})
}

func TestRunnerWaitsForBlockingTextHandlerAndClearsReaction(t *testing.T) {
	f, sink := textPauseFixture(t)
	ready, entered := make(chan struct{}), make(chan struct{})
	f.setTextReply(func(ctx context.Context, key Key, reply store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return TextReplyRetry, ctx.Err()
		case <-ready:
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(reply.Text))
		msg.ID, msg.TaskID = reply.ID, task.ID
		return TextReplyConsumed, f.m.Answer(ctx, key, msg)
	})
	in := inbound("2")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("text handler not called")
	}
	f.agent.mu.Lock()
	sent := len(f.agent.answers)
	f.agent.mu.Unlock()
	if sent != 0 {
		t.Fatal("answer sent before readiness")
	}
	close(ready)
	eventually(t, "answer after readiness", func() bool { f.agent.mu.Lock(); defer f.agent.mu.Unlock(); return len(f.agent.answers) == 1 })
	f.retired(t)
	if got := sink.dequeued(); len(got) != 1 || got[0] != queuedID(in.MessageID)+" "+in.MessageRef {
		t.Fatalf("dequeued notices = %v", got)
	}
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if _, waiting := f.m.marked[in.MessageID]; waiting {
		t.Fatal("consumed answer kept its queued bookkeeping")
	}
}

func TestTextReplyRetriesBackOffAndCap(t *testing.T) {
	f := newFixture(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Second, 4*time.Second
	var delays []time.Duration
	f.m.cfg.After = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		ready := make(chan time.Time, 1)
		ready <- t0
		return ready
	}
	c := &convo{resume: make(chan struct{}, 1)}
	for range 5 {
		if outcome, _ := f.m.waitTextReply(key, c, errors.New("unreachable")); outcome != paused {
			t.Fatalf("retry outcome = %v", outcome)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	for i, d := range want {
		if delays[i] != d {
			t.Errorf("retry %d waits %s, want %s", i, delays[i], d)
		}
	}
}

func TestTextReplyTaskLookupReportsRejectedCredentials(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, agent.ErrCredentialsRejected }
	waited := make(chan struct{}, 1)
	retry := make(chan time.Time)
	f.m.cfg.After = func(time.Duration) <-chan time.Time {
		waited <- struct{}{}
		return retry
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		t.Error("rejected lookup reached decision pipeline")
		return TextReplyWait, nil
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
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not wait for retry")
	}
	retry <- t0
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not retry")
	}
	f.amu.Lock()
	if len(f.rejected) != 1 || !errors.Is(f.rejected[0].Err, agent.ErrCredentialsRejected) {
		t.Errorf("credential rejection bookkeeping = %+v", f.rejected)
	}
	f.amu.Unlock()
	if err := f.m.Cancel(t.Context(), key, "t1", "stop"); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
}

func TestRecoveredTextReplyRetriesBackOff(t *testing.T) {
	f := newFixture(t)
	f.textReplies = true
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Second, 4*time.Second
	r := store.Reply{ID: "recovered-answer", Key: key, UserID: "U1", MessageRef: "2", Text: "answer", EnqueuedAt: t0}
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.EnqueueReply(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	claims := f.st.Store.(interface {
		ClaimReplyInteraction(context.Context, string, string) error
	})
	if err := claims.ClaimReplyInteraction(t.Context(), r.ID, "original-question"); err != nil {
		t.Fatal(err)
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		return TextReplyWait, errors.New("membership unavailable")
	})
	c := f.m.newConvo(key)
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		out, err := f.m.turn(key, c)
		if out != blocked || err == nil || f.m.backoff(c.tries) != want {
			t.Fatalf("recovery retry = %v, %v, %v; want %v", out, err, f.m.backoff(c.tries), want)
		}
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		return TextReplyConsumed, nil
	})
	f.st.mu.Lock()
	f.st.fail["mark"] = errors.New("delivery write unavailable")
	f.st.mu.Unlock()
	if out, err := f.m.turn(key, c); out != blocked || err == nil {
		t.Fatalf("failed flush = %v, %v", out, err)
	}
	if out, err := f.m.turn(key, c); out != idle || err != nil || c.tries != 0 {
		t.Fatalf("recovered flush = %v, %v; tries=%d", out, err, c.tries)
	}

}

func TestTextReplyClearsReactionFromPreviousProcess(t *testing.T) {
	f, sink := textPauseFixture(t)
	// The handler is in place before the reply exists: the runner is still on its way to its park, and a reply it
	// read before then would go to the fixture's default, which judges it no answer and skips it for good.
	f.setTextReply(func(ctx context.Context, k Key, reply store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		msg := answerMsg(reply.ID)
		msg.TaskID = task.ID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	r := store.Reply{ID: "previous-process-answer", Key: key, UserID: "U1", MessageRef: "2", Text: "answer", EnqueuedAt: t0}
	if _, err := f.st.EnqueueReply(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	f.m.start(key) // a prior process’s queued reply is woken by recovery, not a handler setter
	f.retired(t)
	if got := sink.dequeued(); len(got) != 1 || got[0] != queuedID(r.ID)+" "+r.MessageRef {
		t.Fatalf("recovered reply dequeued notices = %v", got)
	}
}

func TestTextPauseRunningTaskLookupFailureRetriesWithoutCancellation(t *testing.T) {
	f, _ := textPauseFixture(t)
	var lookups atomic.Int32
	f.agent.mu.Lock()
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		if lookups.Add(1) == 2 {
			return nil, errors.New("temporary lookup failure")
		}
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &t0}}, nil
	}
	f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	f.agent.mu.Unlock()
	waiting := make(chan struct{}, 1)
	retry := make(chan time.Time, 1)
	f.m.cfg.After = func(time.Duration) <-chan time.Time {
		waiting <- struct{}{}
		return retry
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		t.Error("running task reached the question decider")
		return TextReplyWait, nil
	})
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("transient lookup failure did not back off")
	}
	f.agent.mu.Lock()
	cancelled := len(f.agent.cancels)
	f.agent.mu.Unlock()
	if cancelled != 0 {
		t.Fatal("healthy running task was cancelled after one failed lookup")
	}
	retry <- t0
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.cancels) != 0 || len(f.agent.follows) != 1 || len(f.agent.msgs) != 2 {
		t.Fatalf("cancels=%v follows=%v turns=%d", f.agent.cancels, f.agent.follows, len(f.agent.msgs))
	}
}

func TestSubmitAnswerInterruptsTextReplyBackoff(t *testing.T) {
	f, _ := textPauseFixture(t)
	waiting := make(chan struct{}, 1)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Minute, time.Minute
	f.m.cfg.After = func(time.Duration) <-chan time.Time {
		waiting <- struct{}{}
		return make(chan time.Time) // the answer must wake the runner without this timer firing
	}
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		return TextReplyWait, errors.New("membership lookup unavailable")
	})
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("text reply did not enter backoff")
	}
	msg := answerMsg("submit-answer")
	msg.TaskID = "t1"
	if err := f.m.Answer(t.Context(), key, msg); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.answers) != 1 || f.agent.answers[0].ID != msg.ID || len(f.agent.msgs) != 2 {
		t.Fatalf("answers=%v turns=%d", f.agent.answers, len(f.agent.msgs))
	}
}

// In a mention-only Binding a reply that does not mention Agent Kourier is, while the agent waits on a question that
// takes text replies, that question's answer: it is enqueued pinned to the question, goes through the text-reply
// handler (membership, bot checks, audit), and is never sent as a turn (D38, decision 3).
func TestUnmentionedReplyDuringATextPauseIsPinnedAtEnqueueAndAnswers(t *testing.T) {
	f, _ := textPauseFixtureIn(t, v1alpha1.ThreadRepliesMention)
	f.question("q1", true)
	var seen atomic.Value // the reply the handler was given
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		seen.Store(r)
		msg := answerMsg(r.ID)
		msg.TaskID = task.ID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	in := reply("2", false)
	if res, err := f.submitAny(in); err != nil || res.Disposition != Accepted {
		t.Fatalf("Submit = %+v, %v; want Accepted", res, err)
	}
	f.retired(t)
	got, _ := seen.Load().(store.Reply)
	if got.ID != in.MessageID || got.InteractionID != "q1" {
		t.Errorf("the handler was given %+v; want the reply, pinned to q1 when it was enqueued", got)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.answers) != 1 || f.agent.answers[0].ID != in.MessageID || len(f.agent.msgs) != 1 {
		t.Errorf("answers=%v turns=%d; want the reply sent as the answer and no second turn", f.agent.answers, len(f.agent.msgs))
	}
}

// A pause that takes buttons only, or a thread with no question, leaves an unmentioned reply unmentioned: it is
// never offered to the handler, queued or sent.
func TestUnmentionedReplyDuringANonTextPauseIsUnmentioned(t *testing.T) {
	f, _ := textPauseFixtureIn(t, v1alpha1.ThreadRepliesMention)
	f.question("q1", false)
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		t.Error("a reply that is no answer reached the text-reply handler")
		return TextReplyWait, nil
	})
	if res, err := f.submitAny(reply("2", false)); err != nil || res.Disposition != Unmentioned {
		t.Fatalf("Submit = %+v, %v; want Unmentioned", res, err)
	}
	if err := f.m.Cancel(t.Context(), key, "t1", "stop"); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.answers) != 0 || len(f.agent.msgs) != 1 {
		t.Errorf("answers=%v turns=%d; want the unmentioned reply neither answer nor turn", f.agent.answers, len(f.agent.msgs))
	}
}

// A reply that waits behind a pause of a Binding whose dialect has no text questions is never offered to the
// text-reply handler: it is a turn, and goes out once the pause ends.
func TestAReplyWaitingBehindAPauseWithNoTextQuestionsIsNeverOfferedToTheHandler(t *testing.T) {
	f := newFixture(t) // textReplies is false
	f.m.cfg.RetryBase = time.Millisecond
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	sink := f.live()
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	var offered atomic.Int32
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		offered.Add(1)
		return TextReplyWait, nil
	})
	if _, err := f.submit(reply("2", true)); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Cancel(t.Context(), key, "t1", "stop"); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if offered.Load() != 0 || len(f.agent.answers) != 0 {
		t.Errorf("offered=%d answers=%v; want the reply never offered to the handler and no answer", offered.Load(), f.agent.answers)
	}
}

// A pinned reply whose question closed before the runner reached it (it timed out, or was cancelled or answered
// meanwhile) is consumed at once by the handler, which tells the person, and is never sent as a turn. The turns
// behind it go out without waiting for anything.
func TestPinnedReplyToAClosedQuestionIsConsumedAndBlocksNothing(t *testing.T) {
	// The runner is idle: the question closed, and its task ended, between the intake and the runner's first look
	// at the queue.
	t.Run("no pause", func(t *testing.T) {
		f := newFixture(t)
		f.textReplies, f.threadReplies = true, v1alpha1.ThreadRepliesMention
		f.seed("ns/b1", 0) // the session records task t1, which the question belongs to
		f.question("q1", true)
		typed, mention := reply("2", false), reply("3", true)
		for _, in := range []Inbound{typed, mention} {
			if res, err := f.submitAny(in); err != nil || res.Disposition != Accepted {
				t.Fatalf("Submit = %+v, %v; want Accepted", res, err)
			}
		}
		if err := f.raw.ResolveInteraction(t.Context(), "q1", store.InteractionTimedOut, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		f.setTask("")
		var consumed atomic.Int32
		f.setTextReply(func(_ context.Context, _ Key, r store.Reply, _ a2a.Task) (TextReplyOutcome, error) {
			if r.ID != typed.MessageID || r.InteractionID != "q1" {
				t.Errorf("the handler was given %+v; want only the pinned reply", r)
			}
			consumed.Add(1)
			return TextReplyConsumed, nil // what the pipeline answers for a closed question: the person is told, the press is done
		})
		sink := f.live()
		f.m.start(key)
		waitFinal(t, sink)
		f.retired(t)
		if _, err := f.st.NextReply(t.Context(), key); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the queue still holds a reply: %v", err)
		}
		f.agent.mu.Lock()
		defer f.agent.mu.Unlock()
		if consumed.Load() != 1 || len(f.agent.msgs) != 1 || f.agent.msgs[0].ID != mention.MessageID || len(f.agent.answers) != 0 {
			t.Errorf("consumed=%d turns=%v answers=%v; want the pinned reply consumed, and only the mention sent", consumed.Load(), f.agent.msgs, f.agent.answers)
		}
	})
	// The runner is parked on the question when it closes.
	t.Run("a pause", func(t *testing.T) {
		f, _ := textPauseFixtureIn(t, v1alpha1.ThreadRepliesMention)
		f.question("q1", true)
		typed, mention := reply("2", false), reply("3", true)
		var consumed atomic.Int32
		f.setTextReply(func(_ context.Context, _ Key, r store.Reply, _ a2a.Task) (TextReplyOutcome, error) {
			if r.InteractionID == "" {
				return TextReplyNotAnswer, nil // the mention: not an answer to anything here
			}
			consumed.Add(1)
			return TextReplyConsumed, nil
		})
		for _, in := range []Inbound{typed, mention} {
			if res, err := f.submitAny(in); err != nil || res.Disposition != Accepted {
				t.Fatalf("Submit = %+v, %v; want Accepted", res, err)
			}
		}
		eventually(t, "the pinned reply to be consumed", func() bool { return consumed.Load() == 1 })
		if err := f.m.Cancel(t.Context(), key, "t1", "the question timed out"); err != nil {
			t.Fatal(err)
		}
		f.retired(t)
		f.agent.mu.Lock()
		defer f.agent.mu.Unlock()
		if len(f.agent.msgs) != 2 || f.agent.msgs[1].ID != mention.MessageID || len(f.agent.answers) != 0 {
			t.Errorf("turns=%v answers=%v; want the pinned reply never sent, and the mention sent after the pause", f.agent.msgs, f.agent.answers)
		}
	})
}

// A reply queued pinned is never sent as a turn, whatever the Binding's dialect is by the time the runner reaches it
// (here the Binding was switched to one with no text questions): the handler resolves it by its question.
func TestPinnedReplyIsNeverATurnWhateverTheDialectIs(t *testing.T) {
	f := newFixture(t) // textReplies is false: the Binding's dialect takes no text replies
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	pinned := store.Reply{ID: "pinned", Key: key, UserID: "U1", MessageRef: "2", InteractionID: "q1", Text: "typed answer", EnqueuedAt: t0}
	if _, err := f.st.EnqueueReply(t.Context(), pinned); err != nil {
		t.Fatal(err)
	}
	mention := inbound("3")
	if _, err := f.submit(mention); err != nil {
		t.Fatal(err)
	}
	var consumed atomic.Int32
	f.setTextReply(func(_ context.Context, _ Key, r store.Reply, _ a2a.Task) (TextReplyOutcome, error) {
		if r.ID != pinned.ID || r.InteractionID != "q1" {
			t.Errorf("the handler was given %+v; want only the pinned reply", r)
		}
		consumed.Add(1)
		return TextReplyConsumed, nil
	})
	sink := f.live()
	f.m.start(key)
	waitFinal(t, sink)
	f.retired(t)
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if consumed.Load() != 1 || len(f.agent.msgs) != 1 || f.agent.msgs[0].ID != mention.MessageID {
		t.Errorf("consumed=%d turns=%v; want the pinned reply consumed and only the mention sent", consumed.Load(), f.agent.msgs)
	}
}
