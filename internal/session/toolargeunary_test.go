package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// A resend whose outcome was unknown is looked up first (spec 8.2), because the agent may have the turn and a resend into a context is
// not deduplicated. When the agent's list of tasks cannot be read in full, it is not known whether it has it, and it never will be. The
// turn is not sent: it fails, and each signal is made. The thread is told what happened and that the agent may be running it, with the
// next step; the audit log has the lookup as failed with lookup_incomplete and response_too_large; the log line says why; the outcome
// lookup_incomplete is counted, which an operator can alert on. Nothing is looked up again.
func TestResendWhoseLookupIsIncompleteFailsTheTurnWithEverySignal(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return fmt.Errorf("%w: connection reset", agent.ErrOutcomeUnknown) // the agent may have taken it
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
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
		t.Errorf("%d sends, %d lookups, %d follows; want 2 sends (turn 1, and turn 2 whose outcome was unknown), one lookup and nothing followed: the turn is not sent to find out", sends, finds, follows)
	}
	// The thread.
	got := sink.noticesOf(NoticeFailed)
	if len(got) != 1 || got[0].Detail != lookupIncompleteDetail || got[0].MessageRef != "2" {
		t.Fatalf("failed notices = %+v; want one, about message 2, saying %q", got, lookupIncompleteDetail)
	}
	for _, bad := range []string{"http", "://", "bytes", "Bearer", "4194304"} {
		if strings.Contains(got[0].Detail, bad) {
			t.Errorf("the notice %q holds %q: it names no host, size or token", got[0].Detail, bad)
		}
	}
	if !strings.Contains(got[0].Detail, "may be running it") || !strings.Contains(got[0].Detail, "post it again") {
		t.Errorf("the notice %q does not say that the agent may be running the turn, and what to do", got[0].Detail)
	}
	// The log.
	var failed []logRecord // the retry of the first send's unknown outcome is logged under the same message, without a reason
	for _, line := range logs.lines("session turn failed") {
		if line.attrs["reason"] != nil {
			failed = append(failed, line)
		}
	}
	if len(failed) != 1 || failed[0].attrs["reason"] != lookupIncompleteDetail || failed[0].attrs["binding"] != "ns/b1" || !strings.Contains(fmt.Sprint(failed[0].attrs["error"]), "too large") {
		t.Errorf("failed lines = %+v; want one with the reason, the binding, and an error that says too large", failed)
	}
	// The count.
	if got := ends.get(); len(got) != 2 || got[1] != "ns/b1 "+string(TurnLookupIncomplete) {
		t.Errorf("outcomes reported = %v; want the second turn counted as ns/b1 %s", got, TurnLookupIncomplete)
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
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
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
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != lookupIncompleteDetail {
		t.Errorf("failed notice = %q; want %q", got[0].Detail, lookupIncompleteDetail)
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
	t.Run("the read meets an agent card that is too large", func(t *testing.T) {
		f, sink := pausedFirst(t, unknown)
		f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, fmt.Errorf("get task t1: %w", errCard) }
		if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) >= 1 })
		if got := sink.noticesOf(NoticeFailed); got[0].Detail != answerUnknownDetail {
			t.Errorf("notice = %q; want %q: a card says nothing of what the agent sent, and nothing was cancelled", got[0].Detail, answerUnknownDetail)
		}
		if got := f.cancelled(); len(got) != 0 {
			t.Errorf("cancelled %v; want none: no call was made", got)
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
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
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

// errCard is what a call that had to open a connection first meets when the agent's card is past the bound: nothing was sent, and nothing
// was read of any task.
var errCard = fmt.Errorf("connect to agent: %w: %w: an agent card of more than %d bytes", dialect.ErrCardTooLarge, dialect.ErrResponseTooLarge, dialect.MaxCardBytes)

// A reply that this process queued, and that the agent refused (-32004) or never saw, is not looked up blind: the agent does not have it, so there
// is nothing to find, and when the list of tasks cannot be read in full the reply is sent, as it is whenever the agent is free. It was failed
// with a notice that the agent may be running it, and posting it again failed the same way.
func TestRefusedReplyIsSentWhenTheListCannotBeReadAndTheAgentIsFree(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	var busy atomic.Int32
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 && busy.Add(1) == 1 {
			return fmt.Errorf("send message: %w", agent.ErrBusy) // refused, not recorded: the agent does not have it
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Errorf("failed notices = %+v; want none: the agent never had the reply", got)
	}
	if got := ends.get(); len(got) != 2 || got[1] != "ns/b1 "+string(TurnCompleted) {
		t.Errorf("outcomes reported = %v; want both turns completed", got)
	}
	f.agent.mu.Lock()
	sends := len(f.agent.msgs)
	f.agent.mu.Unlock()
	if sends != 3 {
		t.Errorf("%d sends; want 3: turn 1, turn 2 refused, and turn 2 again", sends)
	}
	if lines := logs.lines("session turn not looked up: the agent's list of tasks could not be read in full, and the agent has not taken the turn"); len(lines) != 1 || lines[0].attrs["binding"] != "ns/b1" {
		t.Errorf("lines = %+v; want the one WARN that says the reply was sent without a lookup, with the binding", lines)
	}
	if !f.queueEmpty() {
		t.Error("the reply is still queued")
	}
}

// The same for a reply that was never sent at all: queued by this process and held behind a task that a restart recorded. When that task ends
// the reply is looked up, the list cannot be read in full, and it is sent: the agent has never had it.
func TestReplyHeldBehindARecordedTaskIsSentWhenTheListCannotBeRead(t *testing.T) {
	f, sink := restarted(t)
	looked := make(chan struct{}, 8)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		looked <- struct{}{}
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
	}
	if _, err := f.submit(inbound("9")); err != nil { // queued by this process, behind the recorded task
		t.Fatal(err)
	}
	select {
	case <-looked:
	case <-time.After(10 * time.Second):
		t.Fatal("the reply was not looked up")
	}
	f.recover(t)
	waitFinal(t, sink) // the recorded task, followed
	waitFinal(t, sink) // the reply, sent after it
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Errorf("failed notices = %+v; want none: the agent never had the reply", got)
	}
	if sends, _, follows := f.agent.counts(); sends != 1 || follows != 1 {
		t.Errorf("%d sends and %d follows; want the task followed once and the reply sent once", sends, follows)
	}
}

// An agent card that is too large is no list that is too large. A lookup that had to open a connection meets it, and nothing was read of the
// agent's tasks: the turn is not failed with a notice about the list, and not sent blind either; it is retried as any turn the agent
// cannot be reached for is, and goes out once the agent can be asked.
func TestLookupThatMeetsAnOversizedCardIsRetriedNotFailed(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if n == 1 {
			return fmt.Errorf("%w: connection reset", agent.ErrOutcomeUnknown)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	var asks atomic.Int32
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		if asks.Add(1) <= 2 {
			return nil, nil, errCard
		}
		return nil, nil, nil // the card is fine again: the agent does not have the turn
	}
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Errorf("failed notices = %+v; want none: a card is no list, and the turn was waiting", got)
	}
	if n := asks.Load(); n != 3 {
		t.Errorf("%d lookups; want 3: two that met the card and one that answered", n)
	}
}

// A task that cannot be followed because the connection could not be opened, for the agent card, is told so. The thread is not told that the
// agent refused, or that what it sent was too large: no call was made. Nothing is cancelled, for there is no connection to cancel on, and the
// task stays recorded.
func TestFollowThatMeetsAnOversizedCardSaysSoAndKeepsTheTask(t *testing.T) {
	f, sink := restarted(t)
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) { return nil, fmt.Errorf("get task t1: %w", errCard) } // a real Agent meets the card on every call
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errCard) }
	f.recover(t)
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != cardTooLargeDetail {
		t.Errorf("notice = %q; want %q", got[0].Detail, cardTooLargeDetail)
	}
	if got := f.cancelled(); len(got) != 0 {
		t.Errorf("cancelled %v; want none: no connection was made", got)
	}
	if id := f.taskID(); id != "t1" {
		t.Errorf("task %q recorded; want t1: the agent still has it", id)
	}
}

// A cancel that could not open its connection did not reach the agent, and is not counted as done. The Stop control reports the failure and
// the record stays, where a cancel whose answer was too large is done.
func TestStopWhoseConnectionCouldNotBeOpenedLeavesTheRecord(t *testing.T) {
	f, _ := restarted(t)
	f.agent.cancelErr = func(int) error { return fmt.Errorf("connect to agent: %w", errCard) }
	stopped, err := f.m.Stop(t.Context(), key, "t1", "U1")
	if err == nil || stopped {
		t.Errorf("Stop = %v, %v; want the failure", stopped, err)
	}
	if id := f.taskID(); id != "t1" {
		t.Errorf("task %q recorded; want t1: the cancel never reached the agent", id)
	}
	if got := classifyCancel(errCard); got != cancelTransient {
		t.Errorf("classifyCancel of a card that is too large = %d, want cancelTransient", got)
	}
}

// A pause that Stop ends on a cancel counted as done without being read says no more than that the thread no longer waits: it does not
// say that the task was stopped, which Agent Kourier does not know. The stop of an ordinary cancel still says it, and a WARN names the
// unverified cancel.
func TestStopOfAPausedTaskThatWasNotVerifiedDoesNotSayItStopped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelErr  error
		getTooBig  bool
		wantNotice NoticeKind
	}{
		{"the cancel's answer was too large", fmt.Errorf("cancel task t1: %w", errAnswerTooLarge), false, NoticeStopUnverified},
		{"the agent says it cannot cancel, and the task is too large to read", fmt.Errorf("cancel task t1: %w", a2a.ErrTaskNotCancelable), true, NoticeStopUnverified},
		{"an ordinary cancel", nil, false, NoticeStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error { return nil })
			logs := newLogRecorder()
			f.m.cfg.Logger = slog.New(logs)
			if tc.cancelErr != nil {
				f.agent.cancelErr = func(int) error { return tc.cancelErr }
			}
			if tc.getTooBig {
				var gets atomic.Int32
				f.agent.get = tooLargeGet(&gets)
			}
			stopped, err := f.m.Stop(t.Context(), key, "t1", "U1")
			if err != nil || !stopped {
				t.Fatalf("Stop = %v, %v; want it stopped", stopped, err)
			}
			other := NoticeStopUnverified
			if tc.wantNotice == NoticeStopUnverified {
				other = NoticeStopped
			}
			if got := sink.noticesOf(tc.wantNotice); len(got) != 1 {
				t.Errorf("%s notices = %d; want one", tc.wantNotice, len(got))
			}
			if got := sink.noticesOf(other); len(got) != 0 {
				t.Errorf("%s notices = %d; want none", other, len(got))
			}
			warned := len(logs.lines("session: the answer to a cancel was too large to read, so the cancel is counted as done without being verified")) +
				len(logs.lines("session: the task could not be read for its size after the agent said it cannot be cancelled, so it is counted as over without being verified"))
			if want := tc.wantNotice == NoticeStopUnverified; (warned == 1) != want {
				t.Errorf("%d WARNs about an unverified cancel; want one = %v", warned, want)
			}
		})
	}
}

// A send that could not open its connection, for the agent card, is a send that never left: it is retried as any is, and it is not failed
// with the notice for what the agent sent, which would be false: the agent was sent nothing and sent nothing.
func TestSendThatMeetsAnOversizedCardIsRetriedNotFailedAsTooLarge(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	var tries atomic.Int32
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		if tries.Add(1) <= 2 {
			return fmt.Errorf("%w: %w", agent.ErrNotSent, errCard)
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if got := sink.noticesOf(NoticeFailed); len(got) != 0 {
		t.Errorf("failed notices = %+v; want none", got)
	}
	if n := tries.Load(); n != 3 {
		t.Errorf("%d sends; want 3: two that never left and one that did", n)
	}
}

// A pause that ends because its task was too large to read also ends the signals that a person made while the task was read: a
// cancel or timeout (endPause) puts one release on the convo's channel, an answer puts its work there, and each channel holds one
// for the park that is over. If one stayed, the next pause of the thread would meet it, and be released, or answered, at once, by a
// person who spoke to the last. Both are put there while the read is in flight, which is where a person's press can come. The reply
// that was read goes out as a turn of its own and pauses on a task of its own, which must stay paused.
func TestPauseThatIsTooLargeToReadLeavesNoSignalForTheNextPause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal func(f *fixture) error
	}{
		{"a cancel or a timeout", func(f *fixture) error { return f.m.endPause(f.t.Context(), key, "t1") }},
		{"an answer", func(f *fixture) error { return f.m.Answer(f.t.Context(), key, answerMsg("ans-1")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, _, ends := logged(t)
			f.textReplies = true
			f.m.cfg.RetryBase = time.Millisecond
			f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
				return turn(h, n, a2a.TaskStateInputRequired) // turn 1 pauses on t1, and the reply that is read pauses on t2
			}
			f.agent.answer = func(*a2a.Message, agent.Handler) error {
				return errors.New("an answer reached the agent") // asserted below: none is sent
			}
			var signalErr error
			f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
				if id == "t1" {
					signalErr = tc.signal(f)
					return nil, fmt.Errorf("get task t1: %w", errAnswerTooLarge)
				}
				return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
			}
			f.setTextReply(func(context.Context, Key, store.Reply, a2a.Task) (TextReplyOutcome, error) {
				return TextReplyWait, nil // for the pause on t2, which the test leaves be
			})
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			waitFinal(t, sink)
			if _, err := f.submit(inbound("2")); err != nil { // the text reply that makes the runner read the task
				t.Fatal(err)
			}
			waitFinal(t, sink) // the reply went out as a turn, and paused on t2
			parked := func() a2a.TaskID {
				f.m.mu.Lock()
				defer f.m.mu.Unlock()
				if c := f.m.convos[key]; c != nil {
					return c.parked
				}
				return ""
			}
			eventually(t, "the runner to park on t2", func() bool { return parked() == "t2" })
			// A signal that was left would act at once. Nothing is awaited that the test can observe, so it looks for a while.
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if p := parked(); p != "t2" {
					t.Fatalf("parked on %q; want the pause on t2 to stay: a signal for the last pause released or answered it", p)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if signalErr != nil {
				t.Errorf("the signal made while the task was read: %v; want it accepted, for the park was not over", signalErr)
			}
			if got := ends.get(); strings.Contains(strings.Join(got, ","), string(TurnPauseEnded)) {
				t.Errorf("outcomes reported = %v; want no pause ended", got)
			}
			f.agent.mu.Lock()
			defer f.agent.mu.Unlock()
			if len(f.agent.answers) != 0 {
				t.Errorf("%d answers sent; want none: the person answered the pause that is over", len(f.agent.answers))
			}
		})
	}
}

// A queued turn that an earlier process left (it may have posted a message for it, a stream that is still open) and that ends here with
// no attempt of its own, because the lookup that has to come first cannot read the agent's list of tasks, hands that on: the sink is given
// the turn, recovered, of the same message, on a context that is already over and does not continue, so that the renderer ends what the
// earlier process showed and does not leave it standing as if the agent were still at work.
func TestLookupIncompleteOnARestartEndsWhatTheEarlierProcessLeft(t *testing.T) {
	f, sink := restarted(t)
	if err := f.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b1", ContextID: "c1", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
	}
	f.recover(t)
	eventually(t, "the head to fail", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != lookupIncompleteDetail {
		t.Errorf("failed notice = %q; want %q", got[0].Detail, lookupIncompleteDetail)
	}
	if sends, _, _ := f.agent.counts(); sends != 0 {
		t.Errorf("%d sends; want none: the head is not sent when it cannot be looked up", sends)
	}
	sink.mu.Lock()
	refs := append([]TurnRef(nil), sink.refs...)
	sink.mu.Unlock()
	if len(refs) != 1 || !refs[0].Recovered || refs[0].MessageID != inbound("1").MessageID {
		t.Fatalf("sink turns %+v; want one, recovered, of message 1: what the earlier process left is ended", refs)
	}
	if cause := sink.causeOf(t, 0); errors.Is(cause, ErrTurnContinues) {
		t.Errorf("the sink context ended with %v; want the turn over, and not continuing", cause)
	}
}

// unverifiedCancelWarning is the WARN that a cancel is counted as done on an answer too large to read (docs/how-to/upgrade.md names it).
const unverifiedCancelWarning = "session: the answer to a cancel was too large to read, so the cancel is counted as done without being verified"

// A cancel that the runner makes itself, of a task whose reply was too large or of one presumed wedged, and whose answer is too large to
// read, is counted as done, and says so in the log: the cancel reached the agent and what it did to the task is not known, and the thread
// is told no more than that the turn stopped or the task was cancelled. Each place that cancels has its own call that logs it.
func TestACancelOfTheRunnerWhoseAnswerIsTooLargeIsWarnedAboutOnce(t *testing.T) {
	cancelErr := func(int) error { return fmt.Errorf("cancel task t1: %w", errAnswerTooLarge) }
	check := func(t *testing.T, f *fixture, logs *logRecorder) {
		t.Helper()
		eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
		f.retired(t)
		lines := logs.lines(unverifiedCancelWarning)
		if len(lines) != 1 || lines[0].attrs["task"] != "t1" {
			t.Errorf("WARNs about an unverified cancel = %+v; want one, for task t1", lines)
		}
		if id := f.taskID(); id != "" {
			t.Errorf("task %q recorded; want it cleared: the cancel is counted as done", id)
		}
		// The audit row says the answer was not read (docs/how-to/upgrade.md): a row that said the cancel worked would claim what is not known.
		f.amu.Lock()
		defer f.amu.Unlock()
		if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || !errors.Is(f.audits[0].Err, dialect.ErrResponseTooLarge) {
			t.Errorf("audits = %+v; want one cancel, failed with response_too_large", f.audits)
		}
	}
	t.Run("a task whose reply was too large", func(t *testing.T) {
		f, _, logs, _ := logged(t)
		f.agent.script = oversized
		f.agent.cancelErr = cancelErr
		if _, err := f.submit(inbound("1")); err != nil {
			t.Fatal(err)
		}
		check(t, f, logs)
	})
	t.Run("a task presumed wedged", func(t *testing.T) {
		logs := newLogRecorder()
		f, _ := wedgedFixture(t, func(f *fixture) {
			f.m.cfg.Logger = slog.New(logs)
			f.agent.cancelErr = cancelErr
		}, "1")
		check(t, f, logs)
	})
}
