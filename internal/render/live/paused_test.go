package live_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// lastChunkOf is the last task_update chunk a streamed message was sent for the step id.
func lastChunkOf(t *testing.T, m fakeslack.Message, id string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, raw := range m.Chunks {
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c["type"] == "task_update" && c["id"] == id {
			last = c
		}
	}
	if last == nil {
		t.Fatalf("no task_update for %s in chunks", id)
	}
	return last
}

// A turn that pauses for a tool approval stops its stream, and a card that is not complete at the stop reads as an error.
// The call that waits for the approval has not failed, so its step is moved on before the stop, under a title that says
// the approval was asked for, and complete: the prompt follows. Its detail is not sent again (a card appends it). The
// answer's own message then shows the call with its real outcome. kagent 1.0 reports the paused call with a "requires
// confirmation" error, which made the step a failure; the a2a dialect reads the same pause.
func TestAStepThatWaitsForApprovalEndsCompleteAndSaysSo(t *testing.T) {
	for _, d := range []struct {
		name string
		d    dialect.Dialect
	}{{"kagent-v1", kagentv1.New(time.Second)}, {"a2a", generic.New(time.Second)}} {
		for _, tc := range []struct {
			name, pause, answer, outcome string
		}{
			{"approved", "approval/01-pause-approve.response.sse", "approval/02-approve.response.sse", "complete"},
			{"rejected", "approval/04-pause-reject-with-reason.response.sse", "approval/05-reject-with-reason.response.sse", "error"},
		} {
			t.Run(d.name+"/"+tc.name, func(t *testing.T) {
				e := setup(t)
				cfg := agent.Config{ParseInteraction: d.d.ParseInteraction}
				pause := recordedEvents(t, tc.pause)
				run := e.stream(t)
				a := agent.New(func(context.Context) (agent.Conn, error) { return recordedConn{events: pause}, nil }, cfg)
				if err := a.Send(t.Context(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("add 3 and 5")), run.Handle); err != nil {
					t.Fatal(err)
				}
				e.finish(t, run)
				paused := e.reply()
				if got := paused.Tasks(); len(got) != 1 || got[0].Title != "Approval requested: add_numbers" || got[0].Status != "complete" || got[0].Details != "a=3, b=5" || got[0].Output != "" {
					t.Fatalf("tasks at the pause %+v; want one, Approval requested: add_numbers, complete, its detail once and no output", got)
				}

				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				answered := e.live.Start(ctx, live.Turn{Source: "ans-1", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputLive, Recipient: "U1"})
				conn := recordedConn{events: recordedEvents(t, tc.answer), paused: pause[len(pause)-1].(*a2a.Task)}
				msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(tc.name))
				msg.TaskID = conn.paused.ID
				if err := agent.New(func(context.Context) (agent.Conn, error) { return conn, nil }, cfg).Answer(t.Context(), msg, answered.Handle); err != nil {
					t.Fatal(err)
				}
				e.finish(t, answered)
				if got := e.reply().Tasks(); len(got) != 1 || got[0].Title != "add_numbers" || got[0].Status != tc.outcome {
					t.Fatalf("tasks of the answer %+v; want add_numbers, %s", got, tc.outcome)
				}
			})
		}
	}
}

// The steps were shown before the pause: the call under way, and the call that waits, failed by the error that stands
// for the pause. Only the calls the pause asks about are retitled, with a last chunk that carries no detail and no output
// (a card appends them, and has its detail from the first chunk). The call under way is ended as any call with no result
// is at a final, and the pause of a question retitles nothing: a call that failed stays failed.
func TestOnlyTheCallsAnApprovalNamesAreRetitled(t *testing.T) {
	pending := map[string]any{"error": "the call requires confirmation"}
	for _, tc := range []struct {
		name     string
		awaiting []string
		want     []fakeslack.Task
	}{
		{"approval", []string{"c2"}, []fakeslack.Task{
			{ID: "s1", Title: "search", Status: "complete", Details: `q="pods"`, Output: "No result was reported."},
			{ID: "s2", Title: "Approval requested: delete_pod", Status: "complete", Details: `name="web-1"`},
		}},
		{"question", nil, []fakeslack.Task{
			{ID: "s1", Title: "search", Status: "complete", Details: `q="pods"`, Output: "No result was reported."},
			{ID: "s2", Title: "delete_pod", Status: "error", Details: `name="web-1"`},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run := e.stream(t)
			run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search", Args: map[string]any{"q": "pods"}})
			run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "delete_pod", Args: map[string]any{"name": "web-1"}})
			run.Handle(agent.ToolResultEvent{CallID: "c2", Response: pending})
			eventually(t, "both steps to be shown, the second failed", func() bool {
				if e.clk.waiting() > 0 {
					e.clk.advance(throttle)
				}
				got := e.tasks()
				return len(got) == 2 && got[1].Status == "error"
			})
			run.Handle(agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, AwaitingApproval: tc.awaiting})
			e.finish(t, run)
			if got := e.tasks(); !slices.Equal(got, tc.want) {
				t.Fatalf("tasks %+v; want %+v", got, tc.want)
			}
			if c := lastChunkOf(t, e.reply(), "s2"); tc.awaiting != nil && (c["status"] != "complete" || c["title"] != "Approval requested: delete_pod" || c["details"] != nil || c["output"] != nil) {
				t.Fatalf("last chunk of the waiting step %v; want complete, retitled, with no details or output", c)
			}
		})
	}
}
