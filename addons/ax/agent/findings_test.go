package agent

import (
	"context"
	"errors"
	"iter"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

// resumeGate holds a resumed turn's model call (one whose last content answers ask_user) until release closes.
type resumeGate struct {
	fakeModel
	resumed chan struct{}
	release chan struct{}
}

func (m *resumeGate) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	last := req.Contents[len(req.Contents)-1]
	for _, p := range last.Parts {
		if p.FunctionResponse == nil {
			continue
		}
		return func(yield func(*model.LLMResponse, error) bool) {
			close(m.resumed)
			select {
			case <-m.release:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
			for r, err := range m.fakeModel.GenerateContent(ctx, req, stream) {
				if !yield(r, err) {
					return
				}
			}
		}
	}
	return m.fakeModel.GenerateContent(ctx, req, stream)
}

// ask pauses a turn on a question and returns its task.
func ask(t *testing.T, a *agentAt) *a2a.Task {
	t.Helper()
	asked := send(t, a.client, message("q1", "ask a question"))
	if asked.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("question: %+v", asked.Status)
	}
	return asked
}

// The kit's request handler holds a send of one message ID until the turn's first event. A resume's first event is
// ADK's working update, which comes before the model call, and the hold is of that message's ID alone, so a resume
// whose model call waits does not hold back a send for another context.
func TestAResumeWaitingOnTheModelDoesNotHoldBackAnotherContext(t *testing.T) {
	m := &resumeGate{resumed: make(chan struct{}), release: make(chan struct{})}
	a := start(t, t.TempDir(), m)
	asked := ask(t, a)
	answer := message("a1", "checkout")
	answer.TaskID, answer.ContextID = asked.ID, asked.ContextID
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		for _, err := range a.client.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: answer}) {
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-m.release:
		default:
			close(m.release)
		}
		<-resumed
	})
	select {
	case <-m.resumed:
	case <-time.After(10 * time.Second):
		t.Fatal("the resume did not reach the model")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, err := range a.client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("m2", "hello")}) {
		if err != nil {
			t.Fatalf("another context's turn while the resume waits on the model: %v", err)
		}
	}
}

// badAnswer answers the paused task with a data part, which the agent refuses with ErrInvalidParams before the turn
// starts (ValidateMessage).
func badAnswer(asked *a2a.Task) *a2a.Message {
	bad := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"not": "text"}))
	bad.ID, bad.TaskID, bad.ContextID = "bad1", asked.ID, asked.ContextID
	return bad
}

// answerOnce sends msg and returns the task as it then stands, and the error the stream ended on, if any.
func answerOnce(t *testing.T, a *agentAt, msg *a2a.Message) (*a2a.Task, error) {
	t.Helper()
	var refusal error
	for _, err := range a.client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			refusal = err
			break
		}
	}
	after, err := a.client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: msg.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	return after, refusal
}

// A malformed answer to a paused task is refused with invalid params before the turn starts (the executor's
// ValidateMessage), and the task stays paused.
func TestAMalformedAnswerIsRefusedAndTheTaskStaysPaused(t *testing.T) {
	a := start(t, t.TempDir(), &fakeModel{})
	after, refusal := answerOnce(t, a, badAnswer(ask(t, a)))
	if refusal == nil || after.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("a malformed answer: refusal %v, the task is %s; want refused and still input-required", refusal, after.Status.State)
	}
}

// detachedStore writes on a cancelled context, as a2a-go's in-memory store does.
type detachedStore struct{ *store.Store }

func (d detachedStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	return d.Store.Update(context.WithoutCancel(ctx), req)
}

// With a store that writes on a cancelled context, the same malformed answer is refused and the task stays paused: the
// refusal rests on the agent, not on the store. The hazard was latent: before the answer was validated, the kit's store kept the
// task paused too, because the executor's error cancels the execution's context before a2a-go writes the task failed,
// and that store does not write on a cancelled context; only a store that does failed the task.
func TestAMalformedAnswerIsRefusedOnAStoreThatIgnoresCancellation(t *testing.T) {
	st, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "ax.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(handler(t, &fakeModel{}, detachedStore{st}, &auth.Check{Verifier: bearerIsCaller{}, Bearer: true, Log: discard()}, nil))
	t.Cleanup(srv.Close)
	a := &agentAt{store: st, http: srv, client: connect(t, srv, "binding")}
	after, refusal := answerOnce(t, a, badAnswer(ask(t, a)))
	if !errors.Is(refusal, a2a.ErrInvalidParams) || after.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("a malformed answer: refusal %v, the task is %s; want invalid params and still input-required", refusal, after.Status.State)
	}
}
