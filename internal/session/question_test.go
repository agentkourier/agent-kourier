package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// askedBy reads a paused task's question as a dialect would: one per status message, named by the message's ID, and "q-A"
// for a status with no message, which is how turn pauses.
func askedBy(task a2a.Task) (*dialect.Interaction, error) {
	request := "q-A"
	if m := task.Status.Message; m != nil {
		request = m.ID
	}
	return &dialect.Interaction{Kind: dialect.AskUser, TaskID: task.ID, RequestID: request, Questions: []dialect.Question{{Text: "Which?"}}}, nil
}

// answerTo is the answer to the question request of task t1, as the interaction module builds it.
func answerTo(request string) *a2a.Message {
	return answerMsg(dialect.AnswerID(questionID(request)))
}

func questionID(request string) string {
	return dialect.InteractionID(dialect.Interaction{Kind: dialect.AskUser, TaskID: "t1", RequestID: request})
}

// pausedAgain is what the agent streams when an answer continues task t1 and the task asks question request next.
func pausedAgain(h agent.Handler, request string) {
	h(agent.StateEvent{TaskID: "t1", ContextID: "c1", State: a2a.TaskStateWorking})
	h(agent.FinalEvent{Task: &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: &a2a.Message{ID: request}}}})
}

// parkedOn waits until the runner of key is parked on task t1 for the question request.
func parkedOn(t *testing.T, f *fixture, request string) {
	t.Helper()
	testutil.Eventually(t, "the runner to park on "+request, func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		c := f.m.convos[key]
		return c != nil && c.parked == "t1" && !c.released && c.paused != nil && c.paused.question == questionID(request)
	})
}

// An answer names the interaction it answers (dialect.AnswerID), and a task takes it only while it is paused on that
// interaction. Once the answer to question A has continued the task and the task asks question B, A's answer sent again (a
// press redelivered, or pressed again on a prompt that kept its controls) is refused, and so is an answer that names no
// question; B's own answer is taken.
func TestAnswerToAnEarlierQuestionOfTheTaskIsRefused(t *testing.T) {
	f := newFixture(t)
	f.parse = askedBy
	sink := f.live()
	pausedFirstOn(t, f, sink, func(msg *a2a.Message, h agent.Handler) error {
		if dialect.AnsweredInteraction(msg.ID) == questionID("q-A") {
			pausedAgain(h, "q-B")
			return nil
		}
		return turn(h, 0, a2a.TaskStateCompleted)
	})
	parkedOn(t, f, "q-A")
	if err := f.m.Answer(t.Context(), key, answerTo("q-B")); !errors.Is(err, ErrNotPaused) {
		t.Errorf("an answer to a question not yet asked = %v; want ErrNotPaused", err)
	}
	if err := f.m.Answer(t.Context(), key, answerTo("q-A")); err != nil {
		t.Fatal(err)
	}
	parkedOn(t, f, "q-B")
	for name, msg := range map[string]*a2a.Message{"the answer to A again": answerTo("q-A"), "an answer to no question": answerMsg("ans-1")} {
		if err := f.m.Answer(t.Context(), key, msg); !errors.Is(err, ErrNotPaused) {
			t.Errorf("%s, with the task paused on B = %v; want ErrNotPaused", name, err)
		}
	}
	if err := f.m.Answer(t.Context(), key, answerTo("q-B")); err != nil {
		t.Fatalf("the answer to B = %v; want it taken", err)
	}
	if id, want := f.nextSent(), inbound("2").MessageID; id != want {
		t.Errorf("sent %s after the answer to B; want the held reply %s", id, want)
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	var got []string
	for _, m := range f.agent.answers {
		got = append(got, m.ID)
	}
	if want := []string{answerTo("q-A").ID, answerTo("q-B").ID}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the agent was sent %v; want A's answer, then B's", got)
	}
}

// A pause that no interaction names (the dialect reads none, or the Backend has no reader) takes no answer that names one.
func TestAnswerToAQuestionIsRefusedByAPauseWithNone(t *testing.T) {
	f, _ := pausedFirst(t, func(_ *a2a.Message, h agent.Handler) error { return turn(h, 0, a2a.TaskStateCompleted) })
	if err := f.m.Answer(t.Context(), key, answerTo("q-A")); !errors.Is(err, ErrNotPaused) {
		t.Errorf("an answer to a question, with the task paused on none = %v; want ErrNotPaused", err)
	}
}

// An answer whose outcome is unknown, to a task that is then read still waiting for input, reopens the pause on the question
// the task is read to wait on: when the answer to A did continue the task and the task asks B, A's answer sent again is
// refused and B's is taken.
func TestAnUnknownAnswerReopensThePauseOnTheQuestionRead(t *testing.T) {
	f := newFixture(t)
	f.parse = askedBy
	sink := f.live()
	pausedFirstOn(t, f, sink, func(msg *a2a.Message, h agent.Handler) error {
		if dialect.AnsweredInteraction(msg.ID) == questionID("q-A") {
			return fmt.Errorf("send message: %w", agent.ErrOutcomeUnknown)
		}
		return turn(h, 0, a2a.TaskStateCompleted)
	})
	f.agent.mu.Lock()
	f.agent.get = func(id a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: &a2a.Message{ID: "q-B"}}}, nil
	}
	f.agent.mu.Unlock()
	if err := f.m.Answer(t.Context(), key, answerTo("q-A")); err != nil {
		t.Fatal(err)
	}
	parkedOn(t, f, "q-B")
	if err := f.m.Answer(t.Context(), key, answerTo("q-A")); !errors.Is(err, ErrNotPaused) {
		t.Errorf("the answer to A again, with the task read waiting on B = %v; want ErrNotPaused", err)
	}
	if err := f.m.Answer(t.Context(), key, answerTo("q-B")); err != nil {
		t.Errorf("the answer to B = %v; want it taken", err)
	}
}

// answerIDFor is the ID of the answer to the question task waits on, as askedBy reads it, as the interaction module names it.
func answerIDFor(task a2a.Task) string {
	b := Backend{ParseInteraction: askedBy}
	return dialect.AnswerID(b.questionOf(task))
}
