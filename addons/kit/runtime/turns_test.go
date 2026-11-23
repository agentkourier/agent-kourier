package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// Each turn is counted once, by how it ended: a cancel wins over what the turn yields after it, and a failed turn is
// the backend's TurnClassifier's to name.
func TestEachTurnIsCountedByHowItEnded(t *testing.T) {
	st := newFakeStore()
	b := &scripted{started: make(chan string, 8)}
	s := serveHandler(t, st, classifying{b})
	c := s.client(t, auth.Caller)
	if _, err := send(t, c, message("m1", "done", "c1", "")); err != nil {
		t.Fatal(err)
	}
	paused, err := send(t, c, message("m2", "pause", "c2", ""))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = send(t, c, message("m3", "bad", "c2", paused.ID)) // without a validator, the turn fails on it
	if _, err := send(t, c, message("m4", "fail", "c3", "")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ids := make(chan a2a.TaskID, 1)
	go func() {
		for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("m5", "hold", "c4", "")}) {
			if err == nil && taskOf(ev) != "" {
				select {
				case ids <- taskOf(ev):
				default:
				}
			}
		}
	}()
	id := <-ids
	if got, err := c.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: id}); err != nil || got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask = %+v, %v", got, err)
	}
	for outcome, want := range map[string]string{"completed": "1", "input_required": "1", "failed": "1", "timeout": "1", "canceled": "1"} {
		wantMetric(t, s.tel, `agentkourier_kit_turns_total{outcome="`+outcome+`"}`, want)
	}
}

func TestOutcomeOf(t *testing.T) {
	failed := a2a.TaskStatus{State: a2a.TaskStateFailed, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("timeout"))}
	ended, end := context.WithCancel(context.Background())
	end()
	for _, tc := range []struct {
		name     string
		o        *outcome
		ctx      context.Context
		classify TurnClassifier
		want     telemetry.TurnOutcome
	}{
		{"completed", &outcome{state: a2a.TaskStateCompleted}, context.Background(), nil, telemetry.TurnCompleted},
		{"completed, then an error", &outcome{state: a2a.TaskStateCompleted, errored: true}, context.Background(), nil, telemetry.TurnFailed},
		{"paused", &outcome{state: a2a.TaskStateInputRequired}, context.Background(), nil, telemetry.TurnInputRequired},
		{"canceled state", &outcome{state: a2a.TaskStateCanceled}, context.Background(), nil, telemetry.TurnCanceled},
		{"a cancel, then completed", canceled(&outcome{state: a2a.TaskStateCompleted}), context.Background(), nil, telemetry.TurnCanceled},
		{"a cancel, then an error", canceled(&outcome{errored: true}), context.Background(), nil, telemetry.TurnCanceled},
		{"failed", &outcome{state: a2a.TaskStateFailed, status: failed}, context.Background(), nil, telemetry.TurnFailed},
		{"failed, classified", &outcome{state: a2a.TaskStateFailed, status: failed}, context.Background(), classifying{}, "timeout"},
		{"failed, not classified", &outcome{state: a2a.TaskStateFailed}, context.Background(), classifying{}, telemetry.TurnFailed},
		{"an error", &outcome{state: a2a.TaskStateWorking, errored: true}, context.Background(), nil, telemetry.TurnFailed},
		{"the caller stopped reading", &outcome{state: a2a.TaskStateWorking, stopped: true}, context.Background(), nil, telemetry.TurnCanceled},
		{"its context ended", &outcome{state: a2a.TaskStateWorking}, ended, nil, telemetry.TurnCanceled},
		{"it just returned", &outcome{state: a2a.TaskStateWorking}, context.Background(), nil, telemetry.TurnFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.of(tc.ctx, tc.classify); got != tc.want {
				t.Fatalf("of = %s, want %s", got, tc.want)
			}
		})
	}
}

func canceled(o *outcome) *outcome {
	o.canceled.Store(true)
	return o
}

// Once stopped, the guarded task store refuses a2a-go's writes and passes its reads on.
func TestGuardedTasksRefuseWritesOnceStopped(t *testing.T) {
	st := newFakeStore()
	st.fakeTask(t, &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	g := &guardedTasks{Store: st}
	g.stop()
	ctx := asCaller(t.Context(), auth.Caller)
	if _, err := g.Create(ctx, &a2a.Task{ID: "t2", ContextID: "c1"}); !errors.Is(err, errStopping) {
		t.Fatalf("Create = %v, want errStopping", err)
	}
	if _, err := g.Get(ctx, "t1"); err != nil {
		t.Fatalf("Get = %v", err)
	}
}
