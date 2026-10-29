package session

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// messageReply is what Agent.Send streams when the agent answers with one Message and no task, in c1.
func messageReply(text string) func(context.Context, int, agent.Handler) error {
	return messageReplyIn(func(int) string { return "c1" }, text)
}

// messageReplyIn is messageReply with the message's contextId for turn n, empty for none: a COMPLETED
// state and final that name no task but carry the context, the message's text, and the message as the
// final task's status, as the assembler folds it.
func messageReplyIn(contextOf func(n int) string, text string) func(context.Context, int, agent.Handler) error {
	return func(_ context.Context, n int, h agent.Handler) error {
		ctxID, id := contextOf(n), fmt.Sprintf("m%d", n)
		h(agent.StateEvent{ContextID: ctxID, State: a2a.TaskStateCompleted})
		var parts a2a.ContentParts
		if text != "" {
			h(agent.TextEvent{ArtifactID: a2a.ArtifactID(id), Text: text})
			parts = a2a.ContentParts{a2a.NewTextPart(text)}
		}
		msg := &a2a.Message{ID: id, ContextID: ctxID, Role: a2a.MessageRoleAgent, Parts: parts}
		h(agent.FinalEvent{Task: &a2a.Task{ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: msg}}})
		return nil
	}
}

// sentContexts returns the contextId of each turn the agent was sent.
func (f *fixture) sentContexts() []string {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	var out []string
	for _, m := range f.agent.msgs {
		out = append(out, m.ContextID)
	}
	return out
}

// A Message that carries no contextId leaves the recorded context as it was, so the next turn still
// continues it. One in another context is the agent's new session for the thread, as for a task: the
// thread is told, and the new context is the one the next turn is sent in (spec 8.2).
func TestMessageReplyContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		second     string // the second reply's contextId
		wantThird  string // the context the third turn is sent in
		wantNotice bool   // the thread is told the agent lost the conversation
	}{
		{"none", "", "c1", false},
		{"another", "c2", "c2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.agent.script = messageReplyIn(func(n int) string {
				if n == 0 {
					return "c1"
				}
				return cmp.Or(tc.second, "c1") // a later turn in the new context replies in it
			}, "the answer")
			for i := range 3 {
				if _, err := f.submit(inbound(fmt.Sprint(i))); err != nil {
					t.Fatal(err)
				}
				waitFinal(t, sink)
				f.retired(t)
			}
			if got, want := f.sentContexts(), []string{"", "c1", tc.wantThird}; !slices.Equal(got, want) {
				t.Errorf("turns sent in contexts %q, want %q", got, want)
			}
			if got := len(sink.noticesOf(NoticeSessionGone)) == 1; got != tc.wantNotice {
				t.Errorf("session-gone notices = %+v; want one: %v", sink.noticesOf(NoticeSessionGone), tc.wantNotice)
			}
		})
	}
}

// An agent that replies to an answer with a Message and no task event has not continued the task with
// it. If the task still waits for input, the pause reopens, as for any answer whose outcome is unknown,
// and the thread is told that the answer was not taken: the agent did reply, so silence would hide that.
// An outcome that is unknown for want of any reply still reopens without a word.
func TestAnswerRepliedToWithAMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		told bool // each answer gets a failed notice that says it was not taken
	}{
		{"a Message", fmt.Errorf("%w: %w", agent.ErrMessageReply, agent.ErrOutcomeUnknown), true},
		{"no reply at all", agent.ErrOutcomeUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := pausedFirst(t, func(*a2a.Message, agent.Handler) error { return tc.err })
			f.agent.mu.Lock()
			f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
				return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &t0}}, nil
			}
			f.agent.mu.Unlock()
			if err := f.m.Answer(t.Context(), key, answerMsg("ans-1")); err != nil {
				t.Fatal(err)
			}
			// Each answer reopens the pause and is answered again: once the third is taken, the first
			// answer's notice, which follows its reopening on the runner's goroutine, has been posted.
			for _, id := range []string{"ans-2", "ans-3"} {
				eventually(t, "the pause to reopen", func() bool { return f.m.Answer(t.Context(), key, answerMsg(id)) == nil })
			}
			got := sink.noticesOf(NoticeFailed)
			if tc.told != (len(got) >= 1) {
				t.Errorf("failed notices = %+v; want some: %v", got, tc.told)
			}
			for _, n := range got {
				if n.Detail != answerMessageDetail {
					t.Errorf("failed notice detail = %q, want %q", n.Detail, answerMessageDetail)
				}
			}
		})
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

// A live working update is life of the task (R17's age), every time: a snapshot never makes one, so it is always news.
func TestAWorkingUpdateRenewsLife(t *testing.T) {
	var l life
	l.renews(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateWorking})
	for range 2 {
		if !l.renews(agent.ProgressEvent{}) {
			t.Error("a working update was not new life")
		}
	}
}
