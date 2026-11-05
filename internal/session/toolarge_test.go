package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// tooLargeSentence is what the thread is told when what the agent sent is more than Agent Kourier can read or hold: an event, an
// answer, the reply, or the tasks of a list. It is fixed, true for each of them, and names neither the backend nor a size: the test
// pins the words, so that a change of them is a decision.
const tooLargeSentence = "the agent sent more than Agent Kourier can read or hold, so the turn stopped"

// errTooLarge is what the agent package returns for a reply past a bound: dialect.ErrResponseTooLarge, with where it
// was found in its text.
var errTooLarge = fmt.Errorf("%w: an event of more than %d bytes", dialect.ErrResponseTooLarge, dialect.MaxEventBytes)

// errAnswerTooLarge is the same for the answer to a call that is not a stream: GetTask, CancelTask or ListTasks.
var errAnswerTooLarge = fmt.Errorf("%w: more than %d bytes", dialect.ErrResponseTooLarge, dialect.MaxResponseBytes)

// oversized makes turn 0 name the task t1, say something, and then fail as agent.Send does when the reply passes a bound:
// no StreamLostError, for the agent would send the same again.
func oversized(_ context.Context, n int, h agent.Handler) error {
	if n > 0 {
		return turn(h, n, a2a.TaskStateCompleted)
	}
	accepted(h, n)
	h(agent.TextEvent{ArtifactID: "a1", Text: "already shown"})
	return fmt.Errorf("send message: %w", errTooLarge)
}

// A reply past what Agent Kourier holds ends the turn with its own notice and outcome, and the task is cancelled at the
// agent, which would otherwise run on with nobody reading it. The agent refused nothing, so the thread is not told it did;
// what was already shown stays. It is no lost stream: nothing is followed again.
func TestResponseTooLargeStopsTheTurnAndCancelsTheTask(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
	}{
		{"a send whose stream passes a bound", func(f *fixture) { f.agent.script = oversized }},
		{"a follow whose stream passes a bound", func(f *fixture) {
			f.agent.script = lost(errors.New("connection reset"))
			f.agent.get = working(t0)
			f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errTooLarge) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, logs, ends := logged(t)
			f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
			tc.setup(f)
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
			f.retired(t)
			got := sink.noticesOf(NoticeFailed)
			if len(got) != 1 || got[0].Detail != tooLargeSentence || got[0].ID != taskNoticeID(key, "t1", NoticeFailed) || got[0].MessageRef != "1" {
				t.Errorf("failed notices = %+v; want one, about message 1, saying %q", got, tooLargeSentence)
			}
			if strings.Contains(got[0].Detail, "refused") {
				t.Errorf("notice %q says the agent refused; it refused nothing", got[0].Detail)
			}
			lines := logs.lines("session turn ended")
			if len(lines) != 1 || lines[0].attrs["outcome"] != string(TurnResponseTooLarge) || lines[0].attrs["task"] != "t1" {
				t.Errorf("end lines = %v; want one, outcome %s, task t1", lines, TurnResponseTooLarge)
			}
			if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
				t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, TurnResponseTooLarge)
			}
			if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
				t.Errorf("cancelled %v; want t1 once: the agent must not go on with a reply nobody reads", got)
			}
			if _, _, follows := f.agent.counts(); follows > 1 {
				t.Errorf("%d follows; want the one that failed: a reply that is too large is no lost stream", follows)
			}
			if id := f.taskID(); id != "" {
				t.Errorf("task %q recorded; want it cleared: it was cancelled, so the next message is not held behind it", id)
			}
			f.amu.Lock()
			defer f.amu.Unlock()
			if len(f.audits) != 1 || f.audits[0].Op != audit.OpCancel || f.audits[0].TaskID != "t1" || f.audits[0].Err != nil || f.actors[0].User != "" {
				t.Errorf("audits = %+v by %+v; want the cancel, by no person: Agent Kourier made it", f.audits, f.actors)
			}
		})
	}
}

// Text that was already shown stays shown: the turn's events up to the failure reach the sink, and the failure is no
// FinalEvent that would replace them.
func TestResponseTooLargeKeepsWhatWasShown(t *testing.T) {
	f, sink, logs, _ := logged(t)
	f.agent.script = oversized
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var text string
	for _, e := range sink.events {
		switch ev := e.(type) {
		case agent.TextEvent:
			text = ev.Text
		case agent.FinalEvent:
			t.Errorf("event %+v; want no final event: the task did not end", ev)
		}
	}
	if text != "already shown" {
		t.Errorf("text %q reached the sink; want what the agent said before the bound", text)
	}
}

// A cancel that fails does not change what the thread is told or how the turn is counted, and the task, which may
// still run, stays recorded, for the Stop control or a restart to reconcile. A task the agent says has ended, or no
// longer has, is cleared: there is nothing left to reconcile.
func TestResponseTooLargeCancelFailureKeepsTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelErr  error
		wantRecord string // the task the session still records
		rejected   int    // Rejected calls: the identity Source hears of a refused credential
	}{
		{"the agent is down", errors.New("agent unreachable"), "t1", 0},
		{"the identity is refused", fmt.Errorf("cancel task t1: %w", agent.ErrForbidden), "t1", 0},
		{"the credentials are refused", fmt.Errorf("cancel task t1: %w", agent.ErrCredentialsRejected), "t1", 1},
		{"the task ended in the meantime", fmt.Errorf("cancel task t1: %w", a2a.ErrTaskNotCancelable), "", 0},
		{"the agent no longer has the task", fmt.Errorf("cancel task t1: %w", a2a.ErrTaskNotFound), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink, logs, ends := logged(t)
			f.agent.script = oversized
			f.agent.cancelErr = func(int) error { return tc.cancelErr }
			if _, err := f.submit(inbound("1")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
			f.retired(t)
			if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
				t.Errorf("failed notices = %+v; want one saying %q whatever the cancel did", got, tooLargeSentence)
			}
			if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
				t.Errorf("outcomes reported = %v; want one, ns/b1 %s whatever the cancel did", got, TurnResponseTooLarge)
			}
			if got := f.cancelled(); len(got) != 1 {
				t.Errorf("cancelled %v; want one try: the turn does not wait on a cancel that fails", got)
			}
			if id := f.taskID(); id != tc.wantRecord {
				t.Errorf("task %q recorded; want %q", id, tc.wantRecord)
			}
			f.amu.Lock()
			defer f.amu.Unlock()
			if len(f.rejected) != tc.rejected {
				t.Errorf("Rejected calls = %+v; want %d", f.rejected, tc.rejected)
			}
			// A refused credential is the identity Source's to record (Rejected); any other failure is the manager's.
			if want := 1 - tc.rejected; len(f.audits) != want || want == 1 && (f.audits[0].Op != audit.OpCancel || !errors.Is(f.audits[0].Err, tc.cancelErr)) {
				t.Errorf("audits = %+v; want %d: the failed cancel, recorded once", f.audits, want)
			}
		})
	}
}

// A cancel that never answers does not hang the turn: it is bounded, and the turn ends as it would have.
func TestResponseTooLargeCancelThatHangsIsBounded(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cancelTimeout = 20 * time.Millisecond
	f.agent.script = oversized
	f.agent.cancelHang = true
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to end although the cancel never answers", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
		t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, TurnResponseTooLarge)
	}
	if id := f.taskID(); id != "t1" {
		t.Errorf("task %q recorded; want t1 kept: the cancel did not get through", id)
	}
}

// The turn behind the one that stopped goes on once the task is cancelled.
func TestResponseTooLargeLetsTheQueueGoOn(t *testing.T) {
	f, sink, _, _ := logged(t)
	f.agent.script = oversized
	for _, ts := range []string{"1", "2"} {
		if _, err := f.submit(inbound(ts)); err != nil {
			t.Fatal(err)
		}
	}
	waitFinal(t, sink) // turn 2 completes, and says so
	if sends, _, _ := f.agent.counts(); sends != 2 {
		t.Errorf("%d sends; want turn 1 and turn 2", sends)
	}
}

// An answer whose stream passes a bound after the task is named again is handled as a send's is: the thread is told in
// the same words, the task is cancelled, and the held reply goes out.
func TestResponseTooLargeOfAnAnswer(t *testing.T) {
	f, sink := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error {
		h(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateWorking})
		return fmt.Errorf("send message: %w", errTooLarge)
	})
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the answer; want %s", id, want)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1 once", got)
	}
}

// A reply that is too large before the agent names a task comes with ErrOutcomeUnknown (no task, so the agent may or may
// not have taken the message). It is no connection that failed: the agent would send the same reply again, and the same
// message would be sent again for as long as SendGiveUp, each time making the agent run it, to end with "the agent could
// not be reached". The turn ends at once instead, with the same notice and outcome, and nothing is sent again or looked up.
func TestResponseTooLargeBeforeATaskIsNamedIsNotRetried(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = func(context.Context, int, agent.Handler) error {
		return fmt.Errorf("%w: %w", agent.ErrOutcomeUnknown, errTooLarge)
	}
	in := inbound("1")
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	f.retired(t)
	got := sink.noticesOf(NoticeFailed)
	if len(got) != 1 || got[0].Detail != tooLargeSentence || got[0].ID != in.MessageID+"/failed" {
		t.Errorf("failed notices = %+v; want one for the turn saying %q", got, tooLargeSentence)
	}
	if got := sink.noticesOf(NoticeRetrying); len(got) != 0 {
		t.Errorf("retrying notices = %+v; want none: nothing is tried again", got)
	}
	if sends, _, _ := f.agent.counts(); sends != 1 {
		t.Errorf("%d sends; want one: the same message would get the same reply", sends)
	}
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
		t.Errorf("outcomes reported = %v; want one, ns/b1 %s", got, TurnResponseTooLarge)
	}
	if got := f.cancelled(); len(got) != 0 {
		t.Errorf("cancelled %v; want none: no task was named", got)
	}
	if next, err := f.st.NextReply(t.Context(), key); err == nil {
		t.Errorf("turn %s is still queued; want it settled: it is not sent again", next.ID)
	}
}

// Through the real client and the stub agent: a reply past the client's budget (agent.DefaultMaxReplyBytes) stops the turn
// with the new notice and outcome, and the task, which the stub holds working, is cancelled there, not left to run.
func TestStubResponseTooLargeCancelsTheTaskAtTheAgent(t *testing.T) {
	e := newStubEnv(t)
	logs, ends := newLogRecorder(), &turnEnds{}
	e.m.cfg.Logger, e.m.cfg.OnTurnEnd = slog.New(logs), ends.add
	e.stub.Next(stubagent.Text(strings.Repeat("x", agent.DefaultMaxReplyBytes+1)), stubagent.Hold("never"))
	if _, err := e.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the end of the turn", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 "+string(TurnResponseTooLarge) {
		t.Fatalf("outcomes reported = %v; want one, ns/b1 %s", got, TurnResponseTooLarge)
	}
	if got := e.sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	sess, _ := e.session()
	if sess.TaskID != "" {
		t.Errorf("task %q recorded; want it cleared once it was cancelled", sess.TaskID)
	}
	task, _, err := e.agent.Agent.FindTask(t.Context(), sess.ContextID, inbound("1").MessageID)
	if err != nil || task == nil || task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("the agent's task = %+v, %v; want it cancelled at the agent", task, err)
	}
}

// An answer whose reply is too large before the task is named again comes with ErrOutcomeUnknown, so the task is read and,
// still working, followed: the follow meets the same reply, and ends as any other turn that does. The answer is not sent
// again, and the task is cancelled.
func TestResponseTooLargeOfAnAnswerBeforeTheTaskIsNamed(t *testing.T) {
	f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error {
		return fmt.Errorf("%w: %w", agent.ErrOutcomeUnknown, errTooLarge)
	})
	f.agent.follow = func(a2a.TaskID, agent.Handler) error { return fmt.Errorf("follow task t1: %w", errTooLarge) }
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the answer; want %s", id, want)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 || got[0].Detail != tooLargeSentence {
		t.Errorf("failed notices = %+v; want one saying %q", got, tooLargeSentence)
	}
	if got := f.cancelled(); len(got) != 1 || got[0] != "t1" {
		t.Errorf("cancelled %v; want t1 once", got)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.answers) != 1 {
		t.Errorf("%d answers sent; want the one: it is not sent again", len(f.agent.answers))
	}
}
