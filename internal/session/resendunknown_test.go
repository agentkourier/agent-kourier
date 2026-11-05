package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// sentIDs are the IDs of the messages the agent was sent, in order.
func (f *fixture) sentIDs() []string {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	var ids []string
	for _, m := range f.agent.msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// A JSON-RPC error that is not busy, before the agent has named a task (-32603, say), says nothing of whether it took the message: it may have
// started the task and failed on the way back. The turn is not a refused one, and a lookup that cannot read the agent's list does not send it
// again to find out: it fails, with the notice. It was sent again, which is the blind resend.
func TestAProtocolErrorBeforeATaskIsNamedIsNotSentAgainWhenTheListCannotBeRead(t *testing.T) {
	f, sink, _, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	var sends atomic.Int32
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		switch sends.Add(1) {
		case 1:
			return turn(h, n, a2a.TaskStateCompleted) // turn 1 gives the session a context
		case 2:
			return fmt.Errorf("send message: %w", a2a.ErrInternalError) // what Agent.Send returns for a -32603 before any event
		}
		return turn(h, n, a2a.TaskStateCompleted)
	}
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second send", func() bool { return sends.Load() == 2 })
	f.retired(t)                                      // the runner retires on the error, with the turn queued
	if _, err := f.submit(inbound("3")); err != nil { // a later message wakes the queue, where "2" is still the head
		t.Fatal(err)
	}
	waitFinal(t, sink) // "3", sent after "2" failed
	f.retired(t)
	if got, want := f.sentIDs(), []string{inbound("1").MessageID, inbound("2").MessageID, inbound("3").MessageID}; !slices.Equal(got, want) {
		t.Errorf("sent %v; want %v: the second message once, never sent again", got, want)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != lookupIncompleteDetail || got[0].MessageRef != "2" {
		t.Errorf("failed notices = %+v; want one, about message 2, saying %q", got, lookupIncompleteDetail)
	}
	if got := ends.get(); !slices.Contains(got, "ns/b1 "+string(TurnLookupIncomplete)) {
		t.Errorf("outcomes reported = %v; want one counted as %s", got, TurnLookupIncomplete)
	}
}

// A turn that the agent took and that was never recorded (m.unrecorded: the agent named a task, and the record of it failed) is held, and is not
// looked up blind: it is the agent's, so a list that cannot be read does not make it a turn that was never sent. It fails, with the notice, and
// is not sent. This is what `hold == nil` in the lookup is for.
func TestATurnTheAgentTookAndNobodyRecordedIsNotSentWhenTheListCannotBeRead(t *testing.T) {
	f, sink, _, _ := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
		return nil, nil, fmt.Errorf("%w: list tasks of context c1: %w", agent.ErrLookupIncomplete, errAnswerTooLarge)
	}
	if _, err := f.submit(inbound("1")); err != nil { // a first turn gives the session a context
		t.Fatal(err)
	}
	waitFinal(t, sink)
	f.m.hold(key, inbound("2").MessageID) // the agent took "2", and the record of it failed
	if _, err := f.submit(inbound("2")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if got := f.sentIDs(); !slices.Equal(got, []string{inbound("1").MessageID}) {
		t.Errorf("sent %v; want only the first message: a turn the agent took is never sent again", got)
	}
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != lookupIncompleteDetail {
		t.Errorf("notice = %q; want %q", got[0].Detail, lookupIncompleteDetail)
	}
}

// A task followed by no one, after a card too large, is not followed by itself; once the card is fixed the session can cancel it (Manager.Stop,
// which has no caller in production yet: the notice names a restart, which TestRestartFollowsTheTaskLeftByACardOnceItIsFixed covers) and the
// queued reply goes out. Every call of a real Agent meets the card, the read
// of the task first, so the fake fails that too: the thread is told at once, and the task is not read again and again until it counts as wedged.
func TestTaskLeftByACardThatIsTooLargeIsReadOnceAndCancellableOnceTheCardIsFixed(t *testing.T) {
	f, sink := restarted(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, 4*time.Millisecond
	var bad atomic.Bool
	bad.Store(true)
	var gets atomic.Int32
	f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
		if bad.Load() {
			gets.Add(1)
			return nil, fmt.Errorf("get task t1: %w", errCard)
		}
		return working(t0)(id)
	}
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errCard) }
	f.agent.cancelErr = func(int) error {
		if bad.Load() {
			return fmt.Errorf("cancel task t1: %w", errCard)
		}
		return nil
	}
	f.recover(t)
	eventually(t, "the notice", func() bool { return len(sink.noticesOf(NoticeFailed)) == 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); got[0].Detail != cardTooLargeDetail {
		t.Errorf("notice = %q; want %q", got[0].Detail, cardTooLargeDetail)
	}
	if !strings.Contains(cardTooLargeDetail, "restart Agent Kourier") || strings.Contains(strings.ToLower(cardTooLargeDetail), "stop") && strings.Contains(cardTooLargeDetail, "press") {
		t.Errorf("the notice %q must say to restart once the card is fixed, and name no control that does not exist", cardTooLargeDetail)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("the task was read %d times; want once: a card is no read to retry until the task counts as wedged", n)
	}
	if _, _, follows := f.agent.counts(); follows != 0 {
		t.Errorf("%d follows; want none: the card could not be read", follows)
	}
	sink.mu.Lock()
	turns := len(sink.refs)
	sink.mu.Unlock()
	if turns != 1 {
		t.Errorf("the sink was given %d turns; want one, recovered, to end the message of the process before", turns)
	}
	// A reply is queued behind the task, which nothing follows, and the card is fixed.
	if _, err := f.submit(inbound("9")); err != nil {
		t.Fatal(err)
	}
	bad.Store(false)
	stopped, err := f.m.Stop(t.Context(), key, "t1", "U1")
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v; want the task cancelled once the card is fixed", stopped, err)
	}
	if id := f.taskID(); id != "" {
		t.Errorf("task %q recorded after Stop; want it cleared", id)
	}
	waitFinal(t, sink) // the queued reply, sent once the task is over
	f.retired(t)
	if got := f.sentIDs(); !slices.Equal(got, []string{inbound("9").MessageID}) {
		t.Errorf("sent %v; want the queued reply", got)
	}
}

// The lookup of the queued head that recovery makes before it follows the recorded task is asked again until it answers. A card that is
// too large fails it the same way each time, until the card is fixed, and what an operator can read is why: one WARN for the thread in a
// run of Recover, whenever the card is first met, and not one for each pass, which would fill the log at the pace of the backoff.
func TestRecoverLogsAnOversizedCardOfTheLookupOnce(t *testing.T) {
	const warning = "recover: the agent's card is too large to read, so the queued head cannot be looked up; it is asked again until the card is fixed"
	card := func() error { return fmt.Errorf("list tasks of context c1: %w", errCard) }
	for _, tc := range []struct {
		name  string
		fails []func() error // the first lookups fail like this, and the one after them answers
	}{
		{"every try meets the card", []func() error{card, card, card, card}},
		{"the first failure is another error", []func() error{func() error { return errors.New("connection refused") }, card, card, card, card}},
		{"the card comes and goes", []func() error{card, func() error { return errors.New("connection refused") }, card, card}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := restarted(t)
			logs := newLogRecorder()
			f.m.cfg.Logger = slog.New(logs)
			queuedBeforeTheRestart(t, f)
			var finds, beforeFollow atomic.Int32
			f.agent.find = func(string, string) (*a2a.Task, []a2a.TaskID, error) {
				if n := int(finds.Add(1)); n <= len(tc.fails) {
					return nil, nil, tc.fails[n-1]()
				}
				return nil, nil, nil // the card is fixed: the head is not the task's turn
			}
			// The runner looks the head up itself once the task has ended, and that is no concern of recovery: the lookups are
			// counted where recovery is done with them, when the recorded task is followed.
			f.agent.follow = func(_ a2a.TaskID, h agent.Handler) error {
				beforeFollow.Store(finds.Load())
				return turn(h, 0, a2a.TaskStateCompleted)
			}
			f.recover(t)
			waitFinal(t, sink) // the recorded task, followed once the lookup answered
			if n, want := int(beforeFollow.Load()), len(tc.fails)+1; n != want {
				t.Fatalf("%d lookups before the follow; want %d: the failures, and the one that answered", n, want)
			}
			lines := logs.lines(warning)
			if len(lines) != 1 || lines[0].attrs["task"] != "t1" || lines[0].attrs["head"] != inbound("1").MessageID {
				t.Errorf("WARNs about the card = %+v; want one, for the head and the task, however many times it was asked", lines)
			}
		})
	}
}
