package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// A follow the agent refuses ends the turn for good: it is not followed again, even when a wake arrives while it
// runs and the runner takes another pass. Turn 2 is submitted while turn 1's follow is in flight, so the runner
// must not retire after the refusal and must not resume the refused follow on its next pass: one Follow call, one
// outcome, one notice for turn 1, and turn 2 stays held behind the recorded task.
func TestRefusedFollowIsNotFollowedAgainWhenAWakeArrivesDuringIt(t *testing.T) {
	f, sink, logs, ends := logged(t)
	f.m.cfg.RetryBase, f.m.cfg.RetryMax = time.Millisecond, time.Millisecond
	f.agent.script = lost(fmt.Errorf("connection reset"))
	f.agent.get = working(t0)
	following, gate := make(chan struct{}), make(chan struct{})
	f.agent.follow = func(a2a.TaskID, agent.Handler) error {
		select {
		case following <- struct{}{}:
		default: // a second follow is the failure; the count shows it
		}
		<-gate
		return fmt.Errorf("follow task t1: %w", agent.ErrForbidden)
	}
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-following:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow never started")
	}
	if _, err := f.submit(inbound("2")); err != nil { // the wake: the runner must go round again after the refusal
		t.Fatal(err)
	}
	close(gate)
	eventually(t, "the end of turn 1", func() bool { return len(logs.lines("session turn ended")) >= 1 })
	f.retired(t)
	if _, _, follows := f.agent.counts(); follows != 1 {
		t.Errorf("%d follows; want the one the agent refused", follows)
	}
	if got := ends.get(); len(got) != 1 || got[0] != "ns/b1 refused" {
		t.Errorf("outcomes reported = %v; want one, ns/b1 refused", got)
	}
	if got := sink.noticesOf(NoticeFailed); len(got) != 1 {
		t.Errorf("failed notices = %+v; want one", got)
	}
}

// The output of an answer is a message of its own, so what the question said does not count for it: text, a pause,
// an answer, and a task that then completes with nothing to show tells the thread so.
func TestAnswerLegWithNoOutputIsToldAfterAQuestionThatSaidSomething(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = func(_ context.Context, n int, h agent.Handler) error {
		id := accepted(h, n)
		h(agent.TextEvent{ArtifactID: "a1", Text: "Which database?"})
		h(agent.FinalEvent{Task: &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}})
		return nil
	}
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) } // says nothing
	if _, err := f.submit(inbound("1")); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	eventually(t, "the runner to park", func() bool { f.m.mu.Lock(); defer f.m.mu.Unlock(); return f.m.convos[key].parked == "t1" })
	if got := sink.noticesOf(NoticeNoOutput); len(got) != 0 {
		t.Fatalf("no-output notices = %+v before the answer; want none: the question said something", got)
	}
	if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the notice for the answer's output", func() bool { return len(sink.noticesOf(NoticeNoOutput)) >= 1 })
	f.retired(t)
	if got := sink.noticesOf(NoticeNoOutput); len(got) != 1 {
		t.Errorf("no-output notices = %+v; want one, for the answer's leg", got)
	}
}
