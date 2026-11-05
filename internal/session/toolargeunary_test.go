package session

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// An answer to a call that is not a stream may pass dialect.MaxResponseBytes: the history of a long task is in it, and
// ListTasks sends it for every task of a context. These are the places where the session meets one, and each ends where a
// reply that is too large always does: no lost stream followed again and again, and no lookup asked for ever.

// A lost stream whose task cannot be read for its size ends with the notice and the outcome of a reply that is too large, the
// task is cancelled, and it is read once: the same answer comes again, so it is not a read to retry until the task counts as
// wedged, which would end the turn as wedged, hours later, with the wrong words.
func TestLostStreamWhoseTaskIsTooLargeToReadEndsWithTheNotice(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	var gets atomic.Int32
	f.agent.script = lost(errors.New("connection reset"))
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		gets.Add(1)
		return nil, fmt.Errorf("get task t1: %w", errAnswerTooLarge)
	}
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

// A task that a restart follows, and whose answer is too large, is read once, ends the turn with the notice and is
// cancelled. When the cancel's own answer is too large too, so that nobody knows whether it took, the task stays recorded
// and the process does not follow it again: the next restart does, finds the cancel refused as ended (the agent had
// cancelled it), and clears the record. Nothing loops in between.
func TestRecoveredTaskWhoseAnswerIsTooLargeIsFollowedOnceAndEnds(t *testing.T) {
	f, sink := restarted(t)
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errAnswerTooLarge) }
	f.agent.cancelErr = func(n int) error {
		if n == 1 {
			return fmt.Errorf("cancel task t1: %w", errAnswerTooLarge)
		}
		return fmt.Errorf("cancel task t1: %w", a2a.ErrTaskNotCancelable)
	}
	f.recover(t)
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	eventually(t, "the cancel", func() bool { return len(f.cancelled()) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != tooLargeSentence {
		t.Errorf("failed notice = %+v; want %q", got[0], tooLargeSentence)
	}
	if _, _, follows := f.agent.counts(); follows != 1 {
		t.Errorf("%d follows; want one: an answer that is too large is no lost stream", follows)
	}
	if id := f.taskID(); id != "t1" {
		t.Fatalf("task %q recorded; want t1: a cancel that may not have taken leaves the task for the next restart", id)
	}
	f.recover(t) // the next restart
	eventually(t, "the record to be cleared", func() bool { return f.taskID() == "" })
	if got := f.cancelled(); len(got) != 2 {
		t.Errorf("cancelled %v; want two tries in all, one for each restart", got)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 2 || got[0].ID != got[1].ID {
		t.Errorf("failed notices = %+v; want one for each restart, under one ID: a repeat after a restart is the same post", got)
	}
}

// Recovery looks the head of the queue up before it goes on, and asks again until it is answered. A page of ListTasks past
// the bound is an answer that never comes, as an agent with no task list is: it used to be asked for for ever, a page of
// megabytes each time. It is no information instead, as that is: the task is followed first, and the head sent after it.
func TestRecoverDoesNotWaitOnALookupWhosePageIsTooLarge(t *testing.T) {
	f, sink := restarted(t)
	queuedBeforeTheRestart(t, f)
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("list tasks of context c1: %w", errAnswerTooLarge)
	}
	f.recover(t)
	waitFinal(t, sink)
	waitFinal(t, sink) // the followed task's, then the head's own
	sends, finds, follows := f.agent.counts()
	if follows != 1 || sends != 1 {
		t.Errorf("%d follows, %d sends; want the task followed once and the head sent once after it", follows, sends)
	}
	if finds > 3 {
		t.Errorf("%d lookups; want a few at most: an answer that cannot come is not asked for again and again", finds)
	}
}

// The runner's lookup of a resend meets the same page: it is taken as no information, as an agent with no task list is, and
// the turn is sent. The turn used to wait, patient, for a lookup that cannot succeed (and be failed as "could not be
// reached" if the agent had not taken it).
func TestResendWhoseLookupPageIsTooLargeIsSentNotWaitedOn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
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
		<-sink.finals
	}
	f.agent.mu.Lock()
	sends, finds, follows := len(f.agent.msgs), len(f.agent.finds), len(f.agent.follows)
	f.agent.mu.Unlock()
	if sends != 3 || finds != 1 || follows != 0 {
		t.Errorf("%d sends, %d lookups, %d follows; want 3 sends (turn 1, turn 2 busy, turn 2 again), one lookup and nothing followed", sends, finds, follows)
	}
}
