package runtime

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

type sent struct {
	task *a2a.Task
	err  error
}

// A send whose backend has not yet yielded its first event holds back a resend of its message ID, which then joins its
// task, and nothing else: a message for another context runs meanwhile.
func TestASendWaitingOnItsBackendHoldsBackOnlyItsOwnMessageID(t *testing.T) {
	st := newFakeStore()
	b := &scripted{gate: make(chan struct{}), started: make(chan string, 4)}
	s := serveHandler(t, st, b)
	c := s.client(t, auth.Caller)
	slow := message("m-slow", "slow", "c1", "")
	first, resent, other := make(chan sent, 1), make(chan sent, 1), make(chan sent, 1)
	go func() { task, err := send(t, c, slow); first <- sent{task, err} }()
	if got := <-b.started; got != "slow" {
		t.Fatalf("the first turn's text = %q", got)
	}
	go func() { task, err := send(t, c, message("m-other", "other", "c2", "")); other <- sent{task, err} }()
	select {
	case r := <-other:
		if r.err != nil || r.task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("the other context's turn = %+v, %v", r.task, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a message for another context waited for a turn whose first event had not come")
	}
	<-b.started // the other turn's
	go func() { task, err := send(t, c, slow); resent <- sent{task, err} }()
	select {
	case r := <-resent:
		t.Fatalf("the resend answered before the first send's first event: %+v, %v", r.task, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(b.gate)
	r1, r2 := <-first, <-resent
	if r1.err != nil || r2.err != nil || r1.task.ID != r2.task.ID || r2.task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("first %+v, %v; resend %+v, %v: want one completed task", r1.task, r1.err, r2.task, r2.err)
	}
	if n := b.turnCount(); n != 2 {
		t.Fatalf("the backend ran %d turns, want 2: the resend joined the first", n)
	}
	wantMetric(t, s.tel, "agentkourier_kit_message_replays_total", "1")
}

// A resend that names another context or another task than its message's is refused; its message's task is unchanged.
func TestAResendNamingAnotherContextOrTaskIsRefused(t *testing.T) {
	st := newFakeStore()
	s := serveHandler(t, st, &scripted{})
	c := s.client(t, auth.Caller)
	task, err := send(t, c, message("m1", "done", "c1", ""))
	if err != nil {
		t.Fatal(err)
	}
	for name, msg := range map[string]*a2a.Message{
		"another context": message("m1", "done", "c2", ""),
		"another task":    message("m1", "done", "c1", "t-other"),
	} {
		if _, err := send(t, c, msg); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("a resend naming %s = %v, want invalid params", name, err)
		}
	}
	if got, err := send(t, c, message("m1", "done", "c1", task.ID)); err != nil || got.ID != task.ID {
		t.Fatalf("the resend naming its own task = %+v, %v", got, err)
	}
}

// A caller cannot follow another caller's running task: a2a-go would stream it by its ID alone.
func TestASubscriptionToAnotherCallersTaskIsNotFound(t *testing.T) {
	st := newFakeStore()
	b := &scripted{started: make(chan string, 1)}
	s := serveHandler(t, st, b)
	owner := s.client(t, "alice")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ids := make(chan a2a.TaskID, 1)
	go func() {
		for ev, err := range owner.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("m1", "hold", "c1", "")}) {
			if err == nil && taskOf(ev) != "" {
				select {
				case ids <- taskOf(ev):
				default:
				}
			}
		}
	}()
	id := <-ids
	t.Cleanup(func() { _, _ = owner.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: id}) })
	followed, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	var got error
	for _, err := range s.client(t, "mallory").SubscribeToTask(followed, &a2a.SubscribeToTaskRequest{ID: id}) {
		got = err
		break
	}
	if !errors.Is(got, a2a.ErrTaskNotFound) {
		t.Fatalf("another caller's subscription = %v, want task not found", got)
	}
}

// An answer the backend's ResumeValidator refuses is refused with invalid params before the turn starts: the task stays
// paused, the backend never sees the answer, and its message ID is not recorded. Without the validator, the same
// refusal from the turn fails the task on a store whose write runs after the request's end.
func TestARefusedAnswerKeepsTheTaskPaused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		backend   func(*scripted) Backend
		wantState a2a.TaskState
	}{
		{"with a validator", func(b *scripted) Backend { return validating{b} }, a2a.TaskStateInputRequired},
		{"without one", func(b *scripted) Backend { return b }, a2a.TaskStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.detached = true
			b := &scripted{}
			s := serveHandler(t, st, tc.backend(b))
			c := s.client(t, auth.Caller)
			paused, err := send(t, c, message("m1", "pause", "c1", ""))
			if err != nil || paused.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("pause = %+v, %v", paused, err)
			}
			_, err = send(t, c, message("m2", "bad", "c1", paused.ID))
			got, getErr := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: paused.ID})
			if getErr != nil || got.Status.State != tc.wantState {
				t.Fatalf("the task after a bad answer = %+v, %v; want %s", got, getErr, tc.wantState)
			}
			if tc.wantState != a2a.TaskStateInputRequired {
				return
			}
			if !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("the bad answer = %v, want invalid params", err)
			}
			if n := b.turnCount(); n != 1 {
				t.Fatalf("the backend ran %d turns, want 1: the refused answer never reached it", n)
			}
			if _, err := send(t, c, message("m2", "bad", "c1", paused.ID)); !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("the bad answer resent = %v, want refused again, not joined to the task", err)
			}
			done, err := send(t, c, message("m3", "fine", "c1", paused.ID))
			if err != nil || done.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("a good answer = %+v, %v", done, err)
			}
			wantMetric(t, s.tel, "agentkourier_kit_message_replays_total", "0")
		})
	}
}

// failingCancel is scripted with a cancel that fails, and a CancelSettler that accepts the task as it stands.
type failingCancel struct {
	*scripted
	asked chan *a2a.Task
}

func (failingCancel) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, errors.New("the backend cannot cancel now")) }
}

func (f failingCancel) CanceledAfterFailedCancel(_ context.Context, task *a2a.Task) bool {
	f.asked <- task
	return true
}

// A cancel that fails is answered with the task as it stands when the backend's CancelSettler accepts it, and fails
// without one.
func TestAFailedCancelIsTheSettlersToAccept(t *testing.T) {
	st := newFakeStore()
	settler := failingCancel{scripted: &scripted{}, asked: make(chan *a2a.Task, 1)}
	s := serveHandler(t, st, settler)
	c := s.client(t, auth.Caller)
	paused, err := send(t, c, message("m1", "pause", "c1", ""))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: paused.ID})
	if err != nil || got.ID != paused.ID {
		t.Fatalf("CancelTask = %+v, %v; want the task the settler accepted", got, err)
	}
	if seen := <-settler.asked; seen.ID != paused.ID {
		t.Fatalf("the settler was asked about %s", seen.ID)
	}

	plain := serveHandler(t, newFakeStore(), struct{ Backend }{failingCancel{scripted: &scripted{}}})
	pc := plain.client(t, auth.Caller)
	paused, err = send(t, pc, message("m1", "pause", "c1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: paused.ID}); err == nil {
		t.Fatal("a failed cancel succeeded without a settler")
	}
}

// A send that is not streamed answers with its task also when the turn resumes a paused task, whose events are updates
// alone.
func TestANonStreamingAnswerIsAnsweredWithItsTask(t *testing.T) {
	s := serveHandler(t, newFakeStore(), &scripted{})
	c := s.client(t, auth.Caller)
	paused, err := send(t, c, message("m1", "pause", "c1", ""))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: message("m2", "fine", "c1", paused.ID)})
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || task.ID != paused.ID || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage = %+v, %v; want the paused task, completed", res, err)
	}
}
