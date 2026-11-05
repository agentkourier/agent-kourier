package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// An answer to a call that is not a stream may pass dialect.MaxResponseBytes: the history of a long task is in it, and
// ListTasks sends it for every task of a page. These are the places where the session meets one, and each ends where what the
// agent sent too much of always does: no lost stream followed again and again, no lookup asked for ever, no turn sent to find
// out, and no thread held behind a task that is over.

// tooLargeGet is a GetTask that answers over the bound, and counts how often it is asked.
func tooLargeGet(n *atomic.Int32) func(a2a.TaskID) (*a2a.Task, error) {
	return func(a2a.TaskID) (*a2a.Task, error) {
		n.Add(1)
		return nil, fmt.Errorf("get task t1: %w", errAnswerTooLarge)
	}
}

// A lost stream whose task cannot be read for its size ends with the notice and the outcome of what the agent sent too much of,
// the task is cancelled, and it is read once: the same answer comes again, so it is not a read to retry until the task counts as
// wedged, which would end the turn as wedged, hours later, with the wrong words.
func TestLostStreamWhoseTaskIsTooLargeToReadEndsWithTheNotice(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	var gets atomic.Int32
	f.agent.script = lost(errors.New("connection reset"))
	f.agent.get = tooLargeGet(&gets)
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if got := sink.noticesOf(NoticeWedged); len(got) != 0 {
		t.Errorf("wedged notices = %+v; want none: the task was not read, so nothing is known of its age", got)
	}
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
		t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, TurnResponseTooLarge)
	}
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1 once", got)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("the task was read %d times; want once: the same answer comes again", n)
	}
}

// A task that a restart finds, and cannot read for its size, ends the turn with the notice, is cancelled, and is read once. The
// message the process before showed is ended too: the sink is given the turn, recovered, as for a task that is gone, or the
// thread would be left with "Working" and a notice.
func TestRecoveredTaskTooLargeToReadEndsWithTheNoticeAndTheSinksTurn(t *testing.T) {
	f, sink := restarted(t)
	var gets atomic.Int32
	f.agent.get = tooLargeGet(&gets)
	f.recover(t)
	eventually(t, "the cancel", func() bool { return len(f.cancelled()) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	sink.mu.Lock()
	refs := append([]TurnRef(nil), sink.refs...)
	sink.mu.Unlock()
	if len(refs) != 1 || !refs[0].Recovered {
		t.Errorf("the sink was given %+v; want one turn, recovered, to end the message of the process before", refs)
	}
	if _, _, follows := f.agent.counts(); follows != 0 || gets.Load() != 1 {
		t.Errorf("%d follows and %d reads; want the task read once and never followed", follows, gets.Load())
	}
	if id := f.taskID(); id != "" {
		t.Errorf("task %q recorded; want it cleared once it was cancelled", id)
	}
}

// An a2a-go server answers every cancel with the whole task, so for a task past the bound each cancel's answer is too large, and
// never the small error that says the task ended (TestRealServerAnswersEveryCancelOfAnOversizedTaskWithTheTask). Such a cancel
// reached the agent: the turn ends and the record is cleared at once, in the process that met it. It was recorded for the next
// restart before, which met the same answer and left it recorded again, so that the thread stayed held for good.
func TestCancelWhoseAnswerIsTheOversizedTaskClearsTheRecordAtOnce(t *testing.T) {
	f, sink := restarted(t)
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errAnswerTooLarge) }
	f.agent.cancelErr = func(int) error { return fmt.Errorf("cancel task t1: %w", errAnswerTooLarge) }
	f.recover(t)
	eventually(t, "the cancel", func() bool { return len(f.cancelled()) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if id := f.taskID(); id != "" {
		t.Errorf("task %q recorded; want it cleared: the cancel reached the agent", id)
	}
	f.recover(t) // a restart has nothing to follow
	if _, _, follows := f.agent.counts(); follows > 1 {
		t.Errorf("%d follows; want the one", follows)
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || !errors.Is(f.audits[0].Err, dialect.ErrResponseTooLarge) {
		t.Errorf("audits = %+v; want the cancel, recorded with the error that says its answer was not read", f.audits)
	}
}

// The Stop control meets the same two answers: a cancel whose answer is the task, and a cancel that says the task ended when the
// task cannot be read to see it. Both are a task that is over, so Stop ends it and clears the record, where it used to report a
// failure each time and leave the thread held.
func TestStopOfATaskTooLargeToReadClearsTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelErr error
	}{
		{"the cancel's answer is the task", fmt.Errorf("cancel task t1: %w", errAnswerTooLarge)},
		{"the cancel says the task ended, and the task is too large to read", fmt.Errorf("cancel task t1: %w", a2a.ErrTaskNotCancelable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := restarted(t)
			var gets atomic.Int32
			f.agent.get = tooLargeGet(&gets)
			f.agent.cancelErr = func(int) error { return tc.cancelErr }
			stopped, err := f.m.Stop(t.Context(), key, "t1", "U1")
			if err != nil || !stopped {
				t.Errorf("Stop = %v, %v; want it stopped", stopped, err)
			}
			if id := f.taskID(); id != "" {
				t.Errorf("task %q recorded; want it cleared", id)
			}
		})
	}
}

// classifyCancel says what a failed cancel means, and a cancel whose answer was too large is a cancel that took.
func TestClassifyCancelOfAnAnswerThatIsTooLarge(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want cancelKind
	}{
		{"nil", nil, cancelDone},
		{"too large", fmt.Errorf("cancel task t1: %w", errAnswerTooLarge), cancelDone},
		{"ended", a2a.ErrTaskNotCancelable, cancelEnded},
		{"gone", a2a.ErrTaskNotFound, cancelGone},
		{"any other failure may pass", errors.New("agent unreachable"), cancelTransient},
	} {
		if got := classifyCancel(tc.err); got != tc.want {
			t.Errorf("%s: classifyCancel = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// In the gap after a crash, the head of the queue is the very task's own turn (the lookup finds it) and the task is too large to
// read. The head is marked delivered with the task ended: the thread is told once. It stayed queued, and was looked up, followed
// and ended again, with a second notice.
func TestHeadThatIsTheOversizedTasksOwnTurnIsToldOnce(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, []a2a.TaskID{"t1"}, nil
	}
	var gets atomic.Int32
	f.agent.get = tooLargeGet(&gets)
	f.recover(t)
	eventually(t, "the cancel", func() bool { return len(f.cancelled()) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 {
		t.Errorf("failed notices = %+v; want one", got)
	}
	if !f.queueEmpty() {
		t.Error("the head is still queued: it is the task's own turn, and the task is over")
	}
	if sends, _, follows := f.agent.counts(); sends != 0 || follows != 0 {
		t.Errorf("%d sends and %d follows; want none", sends, follows)
	}
}

// A reply queued behind a recovered task that is too large to read goes out once the task is over, without another message to wake
// the queue. The turn ended with an error before, and a runner that is handed one retires, which left the reply queued.
func TestReplyBehindAnOversizedRecoveredTaskGoesOutWhenItIsOver(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	var gets atomic.Int32
	f.agent.get = tooLargeGet(&gets)
	f.recover(t)
	waitFinal(t, sink) // the queued reply's own turn, sent after the task was ended
	eventually(t, "the queue to be empty", f.queueEmpty)
	if sends, _, _ := f.agent.counts(); sends != 1 {
		t.Errorf("%d sends; want the queued reply, once", sends)
	}
}

// A resend is looked up first (spec 8.2), because the agent may have the turn and a resend into a context is not deduplicated.
// When the agent's list of tasks is too large to read, at every page size, it is not known whether it has it, and it never will
// be. The turn is not sent: it fails, and each signal is made. The thread is told what happened and that the agent may have
// started it, with the next step; the audit log has the lookup as failed with response_too_large; the log line says why; the
// outcome lookup_too_large is counted, which an operator can alert on. Nothing is looked up again.
func TestResendWhoseLookupIsTooLargeFailsTheTurnWithEverySignal(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return fmt.Errorf("send message: %w", agent.ErrBusy)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("list tasks of context c1: %w", errAnswerTooLarge)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		if ts == "1" {
			waitFinal(t, sink)
		}
	}
	eventually(t, "the end of the second turn", func() bool { return len(logs.lines("session turn ended")) >= 2 })
	f.retired(t)
	f.agent.mu.Lock()
	sends, finds, follows := len(f.agent.msgs), len(f.agent.finds), len(f.agent.follows)
	f.agent.mu.Unlock()
	if sends != 2 || finds != 1 || follows != 0 {
		t.Errorf("%d sends, %d lookups, %d follows; want 2 sends (turn 1, and turn 2 that was busy), one lookup and nothing followed: the turn is not sent to find out", sends, finds, follows)
	}
	// The thread.
	got := sink.noticesOf(NoticeFailed)
	if len(got) != 1 || got[0].Detail != lookupTooLargeDetail || got[0].MessageRef != "2" {
		t.Fatalf("failed notices = %+v; want one, about message 2, saying %q", got, lookupTooLargeDetail)
	}
	for _, bad := range []string{"http", "://", "bytes", "Bearer", "4194304"} {
		if strings.Contains(got[0].Detail, bad) {
			t.Errorf("the notice %q holds %q: it names no host, size or token", got[0].Detail, bad)
		}
	}
	if !strings.Contains(got[0].Detail, "may have started it") || !strings.Contains(got[0].Detail, "post the message again") {
		t.Errorf("the notice %q does not say that the agent may have the turn, and what to do", got[0].Detail)
	}
	// The log.
	failed := logs.lines("session turn failed")
	if len(failed) != 1 || failed[0].attrs["reason"] != lookupTooLargeDetail || !strings.Contains(fmt.Sprint(failed[0].attrs["error"]), "too large") {
		t.Errorf("failed lines = %+v; want one with the reason and an error that says too large", failed)
	}
	// The count.
	if got := ends.get(); len(got) != 2 || got[1] != "ns/b1 "+string(TurnLookupTooLarge) {
		t.Errorf("outcomes reported = %v; want the second turn counted as ns/b1 %s", got, TurnLookupTooLarge)
	}
	// The audit log.
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.audits) != 1 || f.audits[0].Op != audit.OpFindTask || f.audits[0].MessageID != inbound("2").MessageID ||
		!errors.Is(f.audits[0].Err, dialect.ErrResponseTooLarge) {
		t.Errorf("audits = %+v; want one lookup, of message 2, failed with response_too_large", f.audits)
	}
	if !f.queueEmpty() {
		t.Error("the turn is still queued: it failed, and a failed turn is not sent again")
	}
}

// Recovery looks the head of the queue up before it goes on, and asks again until it is answered. A page past the bound at every size
// is an answer that never comes, as it is for an agent with no task list, and it used to be asked for for ever, megabytes each time.
// The recorded task is followed first, as it is then; the head is then looked up by the runner, and fails without being sent: a head
// that was the task's own, after a crash, must not run a second time. Nothing loops.
func TestRecoverDoesNotWaitOnALookupWhosePageIsTooLargeAndDoesNotSendTheHead(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("list tasks of context c1: %w", errAnswerTooLarge)
	}
	f.recover(t)
	waitFinal(t, sink) // the recorded task, followed
	eventually(t, "the head to fail", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	sends, finds, follows := f.agent.counts()
	if follows != 1 || sends != 0 {
		t.Errorf("%d follows, %d sends; want the task followed once and the head never sent", follows, sends)
	}
	if finds > 3 {
		t.Errorf("%d lookups; want a few at most: an answer that cannot come is not asked for again and again", finds)
	}
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != lookupTooLargeDetail {
		t.Errorf("failed notice = %q; want %q", got[0].Detail, lookupTooLargeDetail)
	}
	if !f.queueEmpty() {
		t.Error("the head is still queued: it failed")
	}
}

// A person's text reply to a paused task reads the task first, and a task too large to read ends the turn as any other does:
// the thread is told, the task is cancelled and the pause ends. It was read again after every backoff until the pause ended.
func TestTextReplyToAPausedTaskThatIsTooLargeToReadEndsTheTurn(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.textReplies = true
	f.m.cfg.RetryBase = time.Millisecond
	release := make(chan struct{})
	f.agent.script = func(ctx context.Context, n int, h agent.Handler) error {
		if n == 0 {
			return pauseFirst(ctx, n, h)
		}
		<-release // the reply that was read goes out as a turn of its own, once the test has looked at the pause
		return turn(h, n, a2a.TaskStateCompleted)
	}
	var gets atomic.Int32
	f.agent.get = tooLargeGet(&gets)
	f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
		t.Error("a task that cannot be read reached the decision")
		return TextReplyWait, nil
	})
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
	// The pause is over, and the runner holds nothing while it goes on to the reply, which waits for the release.
	eventually(t, "the runner to let go of the task t1", func() bool { return !f.m.HoldsTask(key, "t1") })
	close(release)
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("the task was read %d times; want once", n)
	}
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1 once", got)
	}
	if got := ends.get(); !strings.Contains(strings.Join(got, ","), "ns/b1 "+string(TurnResponseTooLarge)) || strings.Contains(strings.Join(got, ","), string(TurnPauseEnded)) {
		t.Errorf("outcomes reported = %v; want the turn counted as %s once, not as a pause that ended", got, TurnResponseTooLarge)
	}
	_ = logs
}

// An answer whose outcome is unknown reads the task, and a read that fails leaves it unknown: the thread is not told that the answer
// could not be delivered, when the agent may have taken it. A task too large to read ends the turn as any other does.
func TestAnswerOfUnknownOutcomeWhoseTaskCannotBeRead(t *testing.T) {
	unknown := func(*a2a.Message, agent.Handler) error {
		return fmt.Errorf("%w: connection reset", agent.ErrOutcomeUnknown)
	}
	t.Run("the read fails", func(t *testing.T) {
		f, sink := pausedFirst(t, unknown)
		f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, errors.New("agent unreachable") }
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
		got := sink.noticesOf(NoticeFailed)
		if got[0].Detail != "Agent Kourier could not tell whether the agent took the answer" {
			t.Errorf("notice = %q; want that it could not tell, and no claim that the answer was not delivered", got[0].Detail)
		}
	})
	t.Run("the task is too large to read", func(t *testing.T) {
		f, sink := pausedFirst(t, unknown)
		var gets atomic.Int32
		f.agent.get = tooLargeGet(&gets)
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
		if got := sink.noticesOf(NoticeFailed); got[0].Detail != tooLargeSentence {
			t.Errorf("notice = %q; want %q", got[0].Detail, tooLargeSentence)
		}
		eventually(t, "the cancel", func() bool { return len(f.cancelled()) >= 1 }) // the thread is told first
		if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
			t.Errorf("cancelled %v; want t1 once", got)
		}
	})
}

// A queued head whose lookup is too large to read, while the session records a task, is not failed: it was not sent, and it waits
// behind the task. The runner that is woken before recovery has attached the task meets this, and holds the head; the task is then
// followed, and the head is looked up again when it ends, and fails then.
func TestLookupTooLargeBehindARecordedTaskHoldsTheHeadUntilTheTaskEnds(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	looked := make(chan struct{}, 8)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		looked <- struct{}{}
		return nil, nil, fmt.Errorf("list tasks of context c1: %w", errAnswerTooLarge)
	}
	if _, err := f.submit(inbound("1")); err != nil { // the redelivered event wakes a runner before recovery attaches the task
		t.Fatal(err)
	}
	select {
	case <-looked:
	case <-time.After(10 * time.Second):
		t.Fatal("the head was not looked up")
	}
	f.retired(t) // the runner held the head and has nothing to do until the task ends
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Fatalf("failed notices = %+v; want none: the head waits behind the recorded task", got)
	}
	if f.queueEmpty() {
		t.Fatal("the head left the queue while the task it waits behind still ran")
	}
	f.recover(t)
	waitFinal(t, sink) // the recorded task, followed
	eventually(t, "the head to fail", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if sends, _, follows := f.agent.counts(); sends != 0 || follows != 1 {
		t.Errorf("%d sends and %d follows; want the task followed once and the head never sent", sends, follows)
	}
}
