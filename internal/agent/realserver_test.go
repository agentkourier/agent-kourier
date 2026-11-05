package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// bigTasks is the executor of a task that has grown past dialect.MaxResponseBytes: five tool results of a MiB each, as
// artifacts, and then it works on until it is cancelled.
type bigTasks struct{ started chan a2a.TaskID }

func (e *bigTasks) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		blob := strings.Repeat("x", 1<<20)
		for i := range 5 {
			ev := a2a.NewArtifactEvent(ec, a2a.NewDataPart(map[string]any{"id": fmt.Sprint("c", i), "name": "kubectl", "response": map[string]any{"output": blob}}))
			ev.Artifact.Parts[0].Metadata = map[string]any{"kagent.dev/a2a/part-type": "function_response"}
			if !yield(ev, nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		e.started <- ec.TaskID
		<-ctx.Done()
	}
}

func (e *bigTasks) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// A real a2a-go server answers every cancel of a task, a repeat of one that already took included, with the whole task, so for a
// task past the bound the answer to CancelTask is too large every time, and never the "cannot be cancelled" error that is small.
// The session relies on it: a cancel whose answer is too large is a cancel that reached the agent, and the same cancel again
// would be no better. The task is read the same way, too large, however many times it is asked for.
func TestRealServerAnswersEveryCancelOfAnOversizedTaskWithTheTask(t *testing.T) {
	ex := &bigTasks{started: make(chan a2a.TaskID, 1)}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	card := &a2a.AgentCard{Name: "ask", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)}}
	card.Capabilities.Streaming = true
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(ex)))
	// A real server and 5 MiB of tasks on a loaded runner: the budget is for the test to finish, not for anything to be quick, so it is
	// minutes, and a call is given as long (the 30 s it had failed every run at a load average of 60).
	slow := quick
	slow.UnaryTimeout = 3 * time.Minute
	a := (&fakeAgent{Server: srv}).agentViaWith(t, generic.New(0), slow)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	go func() { _ = a.Send(ctx, message("go"), nil) }()
	var id a2a.TaskID
	select {
	case id = <-ex.started:
	case <-ctx.Done():
		t.Fatal("the task did not start")
	}
	for { // the server stores the task as it takes the events: read it until it is the size it will be
		if _, err := a.GetTask(ctx, id); err != nil && !errors.Is(err, a2a.ErrTaskNotFound) { // not stored yet is not an answer
			if !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("GetTask = %v, want the task or an answer that is too large", err)
			}
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the task never passed the bound")
		}
	}
	for i := 1; i <= 3; i++ {
		task, err := a.CancelTask(ctx, id)
		if !errors.Is(err, dialect.ErrResponseTooLarge) || errors.Is(err, a2a.ErrTaskNotCancelable) || task != nil {
			t.Errorf("CancelTask %d = %v, %v; want an answer that is too large every time, as the whole task", i, task, err)
		}
	}
	if _, err := a.GetTask(ctx, id); !errors.Is(err, dialect.ErrResponseTooLarge) {
		t.Errorf("GetTask after the cancels = %v, want an answer that is too large", err)
	}
}
