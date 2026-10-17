package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// messageReply is what Agent.Send streams when the agent answers with one Message and no task:
// a COMPLETED state and final that name no task but carry the context, and the message's text.
func messageReply(text string) func(context.Context, int, agent.Handler) error {
	return func(_ context.Context, n int, h agent.Handler) error {
		h(agent.StateEvent{ContextID: "c1", State: a2a.TaskStateCompleted})
		if text != "" {
			h(agent.TextEvent{ArtifactID: a2a.ArtifactID(fmt.Sprintf("m%d", n)), Text: text})
		}
		h(agent.FinalEvent{Task: &a2a.Task{ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
		return nil
	}
}

// A turn the agent answers with a Message has no task to record or recover: the session keeps
// the contextId alone, the turn leaves the queue, and the next turn continues the context (spec 8.2).
func TestMessageReplyKeepsTheContextAndNoTask(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = messageReply("the answer")
	for i, wantContext := range []string{"", "c1"} {
		if _, err := f.submit(inbound(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		f.retired(t)
		sess, err := f.session()
		if err != nil || sess.ContextID != "c1" || sess.TaskID != "" {
			t.Fatalf("turn %d: session = %+v, %v; want context c1 and no task", i, sess, err)
		}
		f.agent.mu.Lock()
		got := f.agent.msgs[i].ContextID
		f.agent.mu.Unlock()
		if got != wantContext {
			t.Errorf("turn %d sent in context %q, want %q", i, got, wantContext)
		}
	}
	f.agent.mu.Lock()
	n := len(f.agent.msgs)
	f.agent.mu.Unlock()
	if n != 2 {
		t.Errorf("sends = %d, want 2: a turn the agent answered is never sent again", n)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.notices) != 0 {
		t.Errorf("notices = %+v; want none for a reply with text", sink.notices)
	}
}

// A Message with nothing to read is told, like a task that completed with no output. With no task to
// name the notice after it is the turn's, so a second such reply in the thread is told too.
func TestEmptyMessageReplyIsToldPerTurn(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.agent.script = messageReply("")
	for i := range 2 {
		if _, err := f.submit(inbound(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		waitFinal(t, sink)
		f.retired(t)
	}
	got := sink.noticesOf(NoticeNoOutput)
	if len(got) != 2 || got[0].ID == got[1].ID {
		t.Errorf("no-output notices = %+v; want two, one per turn", got)
	}
}

// Each reply with no task is a first look: the life held for the thread starts again, so no reply's
// text outlives its turn, after a task turn too.
func TestLifeStartsAgainForEachMessageReply(t *testing.T) {
	var l life
	l.renews(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateCompleted})
	for _, id := range []a2a.ArtifactID{"m1", "m2"} {
		if !l.renews(agent.StateEvent{ContextID: "c1", State: a2a.TaskStateCompleted}) {
			t.Errorf("reply %s: the state was not a first look", id)
		}
		l.renews(agent.TextEvent{ArtifactID: id, Text: "the answer"})
		if len(l.text) != 1 {
			t.Errorf("reply %s: life holds %d texts, want only this reply's", id, len(l.text))
		}
	}
}
