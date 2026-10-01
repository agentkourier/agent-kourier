package interact_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// sess records what the intake asks of the session manager.
type sess struct {
	answers              []*a2a.Message
	cancels              []a2a.TaskID
	answerErr, cancelErr error
	onAnswer             func() // runs as an answer arrives
}

func (s *sess) Answer(_ context.Context, _ store.ConversationKey, m *a2a.Message) error {
	s.answers = append(s.answers, m)
	if s.onAnswer != nil {
		s.onAnswer()
	}
	return s.answerErr
}

func (s *sess) Cancel(_ context.Context, _ store.ConversationKey, id a2a.TaskID, _ string) error {
	s.cancels = append(s.cancels, id)
	return s.cancelErr
}

// TestRecoveryByState: the pause comes again, and what happens depends on the interaction's state.
func TestRecoveryByState(t *testing.T) {
	task := askUser("req-1")
	answer, _ := json.Marshal(dialect.Answer{RequestID: "req-1", Answers: [][]string{{"a"}}})
	answered := func(r *rig, row store.Interaction) {
		_ = r.st.SubmitInteraction(t.Context(), row.ID, answer, "U1", t0.Add(time.Minute))
	}
	timedOut := func(r *rig, row store.Interaction) {
		_ = r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionTimedOut, t0.Add(timeout))
	}
	for _, tc := range []struct {
		name                    string
		decide                  func(*rig, store.Interaction)
		answerErr, cancelErr    error
		wantAnswers, wantCancel int
	}{
		{"pending with a prompt is left alone", nil, nil, nil, 0, 0},
		{"answered is sent again", answered, nil, nil, 1, 0},
		{"answered is dropped once the task has moved on", answered, session.ErrNotPaused, nil, 1, 0},
		{"timed out is cancelled again", timedOut, nil, nil, 0, 1},
		{"timed out whose pause has ended is left", timedOut, nil, session.ErrNotPaused, 0, 1},
		{"cancelled is cancelled again", func(r *rig, row store.Interaction) {
			_ = r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionCancelled, t0.Add(time.Minute))
		}, nil, nil, 0, 1},
		{"cancelled whose pause has ended is left", func(r *rig, row store.Interaction) {
			_ = r.st.ResolveInteraction(t.Context(), row.ID, store.InteractionCancelled, t0.Add(time.Minute))
		}, nil, session.ErrNotPaused, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.sess.answerErr, r.sess.cancelErr = tc.answerErr, tc.cancelErr
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			if tc.decide != nil {
				tc.decide(r, r.row(t, task))
			}
			if err := r.paused(t, task); err != nil {
				t.Fatalf("Paused again = %v", err)
			}
			if len(r.sess.answers) != tc.wantAnswers || len(r.sess.cancels) != tc.wantCancel || len(r.asked) != 1 {
				t.Errorf("%d answers, %d cancels, %d prompts rendered; want %d, %d, 1", len(r.sess.answers), len(r.sess.cancels), len(r.asked), tc.wantAnswers, tc.wantCancel)
			}
			if tc.wantAnswers > 0 {
				m := r.sess.answers[0]
				actor, _ := m.Metadata[kagentv1.ActorMetadataKey].(map[string]any)
				if m.TaskID != "task-1" || m.ID != r.row(t, task).ID+"/answer" || m.Metadata[kagentv1.HITLExtension] == nil || actor["id"] != "U1" {
					t.Errorf("answer message = %+v; want it for the task, with the interaction's message ID, the payload, and U1, who decided, as the actor", m)
				}
			}
		})
	}
}

// TestAnAnswerIsNeverSentToADifferentQuestion: the request ID is the model's tool call ID and may be
// reused (UNCONFIRMED(WP-02)). A person answered question A; a pause under the same ID with other
// questions is not A, so the answer is not sent as its answer.
func TestAnAnswerIsNeverSentToADifferentQuestion(t *testing.T) {
	r, first := newRig(t, nil), askUser("req-1")
	if err := r.paused(t, first); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(dialect.Answer{RequestID: "req-1", Answers: [][]string{{"a"}}})
	if err := r.st.SubmitInteraction(t.Context(), r.row(t, first).ID, answer, "U1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	other := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-1", "questions": []map[string]any{{"question": "Delete the database?", "choices": []string{"a", "b"}}}})
	if err := r.paused(t, other); err != nil || len(r.sess.answers) != 0 || len(r.sess.cancels) != 1 {
		t.Errorf("Paused = %v after %d answers sent and %d cancels; want no answer, and the task cancelled as unusable", err, len(r.sess.answers), len(r.sess.cancels))
	}
}

// An answer sent again after a restart is marked delivered when the session takes it, and not when the task
// has moved on, which says nothing of whether the first send got through.
func TestARecoveredAnswerIsMarkedDeliveredWhenTheSessionTakesIt(t *testing.T) {
	task := askUser("req-1")
	answer, _ := json.Marshal(dialect.Answer{RequestID: "req-1", Answers: [][]string{{"a"}}})
	for name, tc := range map[string]struct {
		answerErr error
		marked    bool
	}{"taken": {nil, true}, "the task has moved on": {session.ErrNotPaused, false}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			_ = r.st.SubmitInteraction(t.Context(), r.row(t, task).ID, answer, "U1", t0.Add(time.Minute))
			r.sess.answerErr = tc.answerErr
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			if got := r.row(t, task); got.DeliveredAt.IsZero() == tc.marked {
				t.Errorf("delivered at %v; want it marked: %t", got.DeliveredAt, tc.marked)
			}
		})
	}
}
