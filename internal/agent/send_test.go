package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// collector returns a Handler and the events it has been given.
func collector() (Handler, *[]Event) {
	var got []Event
	return func(e Event) { got = append(got, e) }, &got
}

func TestSendStreamsTheFinalTextAndTask(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", -1) })
	h, got := collector()
	msg := message("go slowly")
	id := msg.ID
	if err := fa.agent(t).Send(t.Context(), msg, h); err != nil {
		t.Fatal(err)
	}
	all := texts(*got)
	if len(all) != 8 || all[7] != ticks {
		t.Errorf("texts = %q, want eight growing texts ending in %q", all, ticks)
	}
	if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("last event = %#v, want the COMPLETED task", (*got)[len(*got)-1])
	}
	if ids := fa.sentMessageIDs(t); !slices.Equal(ids, []string{id}) {
		t.Errorf("sent message IDs = %v, want [%s]", ids, id)
	}
}

// After WORKING, kagent reports a bad answer as an error event over HTTP 200 (spec 6.3). It
// is the send's outcome, and an answer's failure is typed so the caller can CancelTask (R17).
func TestErrorEventMidStreamIsTheOutcome(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { sse(t, w, "askuser/10-bad-empty-answers.response.sse", -1) })
	for _, tc := range []struct {
		name       string
		send       func(*Agent, context.Context, *a2a.Message, Handler) error
		wantTyped  bool
		wantStates []a2a.TaskState
	}{
		{"answer", (*Agent).Answer, true, []a2a.TaskState{a2a.TaskStateWorking}},
		{"send", (*Agent).Send, false, []a2a.TaskState{a2a.TaskStateWorking}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, got := collector()
			err := tc.send(fa.agent(t), t.Context(), message("nope"), h)
			if errors.Is(err, ErrAnswerRejected) != tc.wantTyped {
				t.Errorf("errors.Is(err, ErrAnswerRejected) = %v, want %v; err = %v", !tc.wantTyped, tc.wantTyped, err)
			}
			if !errors.Is(err, a2a.ErrInternalError) {
				t.Errorf("err = %v, want it to carry the agent's -32603", err)
			}
			if !slices.Equal(states(*got), tc.wantStates) {
				t.Errorf("states = %v, want %v delivered before the error", states(*got), tc.wantStates)
			}
		})
	}
}

// A rejected message is not recorded, so the queue resends it unchanged, same messageId,
// once the task ends (spec 8.2). Send must neither retry it nor change it.
func TestBusyIsTypedAndTheMessageCanBeResentUnchanged(t *testing.T) {
	var sends atomic.Int32
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
		if sends.Add(1) == 1 {
			sse(t, w, "busy/03-second-SendStreamingMessage.response.sse", -1)
			return
		}
		sse(t, w, "busy/09-second-resend-after-completion.response.sse", -1)
	})
	agent, msg := fa.agent(t), message("second")
	h, got := collector()
	err := agent.Send(t.Context(), msg, h)
	if !errors.Is(err, ErrBusy) || errors.Is(err, ErrForbidden) || len(*got) != 0 {
		t.Fatalf("err = %v, events = %v; want ErrBusy and no events", err, *got)
	}
	if n := fa.count("SendStreamingMessage"); n != 1 {
		t.Errorf("agent got %d sends, want 1: -32004 is not retried immediately", n)
	}
	if err := agent.Send(t.Context(), msg, h); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if ids := fa.sentMessageIDs(t); len(ids) != 2 || ids[0] != msg.ID || ids[1] != msg.ID {
		t.Errorf("sent message IDs = %v, want %s twice", ids, msg.ID)
	}
}

func TestForbiddenIsTyped(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
		sse(t, w, "identity/06-continue-other-email-other-sub.response.sse", -1)
	})
	err := fa.agent(t).Send(t.Context(), message("hi"), nil)
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

// rpcError is a JSON-RPC error event as kagent sends it: SSE over HTTP 200.
func rpcError(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		write(w, []byte(fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"error\":{\"code\":%d,\"message\":\"x\"}}\n\n", code)))
	}
}

// a2a-go turns every code it does not know into -32603, so an answer that fails with any
// of them is typed as rejected, and the caller cancels the task (R17).
func TestAnswerErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		want    error
		notWant error
	}{
		{"busy, wedged or ended", -32004, ErrBusy, ErrAnswerRejected},
		{"forbidden", -31403, ErrForbidden, ErrAnswerRejected},
		{"internal error", -32603, ErrAnswerRejected, ErrBusy},
		{"a code a2a-go does not know", -32050, ErrAnswerRejected, ErrBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { rpcError(tc.code)(w) })
			err := fa.agent(t).Answer(t.Context(), message("yes"), nil)
			if !errors.Is(err, tc.want) || errors.Is(err, tc.notWant) {
				t.Errorf("err = %v, want %v and not %v", err, tc.want, tc.notWant)
			}
		})
	}
}

// A send replayed after a crash, with the same messageId and no contextId, gets one of two
// answers (WP-02): a live stream of the running task, or a snapshot of the finished one.
func TestReplayedSendJoinsTheRunningTaskOrGetsItsSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		wantTexts     []string
	}{
		{"running", "inflight/02-a-replay-while-running.response.sse", []string{"tick 1 ", "tick 1 tick 2 ", "tick 1 tick 2 tick 3 ", "tick 1 tick 2 tick 3 tick 4 ", "tick 1 tick 2 tick 3 tick 4 tick 5 ", "tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 ", "tick 1 tick 2 tick 3 tick 4 tick 5 tick 6 tick 7 ", ticks}},
		{"finished", "session/02-replay-same-messageid.response.sse", []string{"The answer is 4."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { sse(t, w, tc.fixture, -1) })
			h, got := collector()
			if err := fa.agent(t).Send(t.Context(), message("go slowly"), h); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(texts(*got), tc.wantTexts) {
				t.Errorf("texts = %q, want %q", texts(*got), tc.wantTexts)
			}
			if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task.Status.State != a2a.TaskStateCompleted {
				t.Errorf("last event = %#v, want the COMPLETED task", (*got)[len(*got)-1])
			}
		})
	}
}

func TestLostStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(http.ResponseWriter)
		want  error // ErrStreamLost only once the agent has named a task, else ErrOutcomeUnknown
	}{
		{"reset after the task began, and no reconnect works", func(w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", 4); drop() }, ErrStreamLost},
		{"ended cleanly before the task settled, and no reconnect works", func(w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", 4) }, ErrStreamLost},
		{"reset before any event", func(http.ResponseWriter) { drop() }, ErrOutcomeUnknown},
		{"server error", func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusBadGateway) }, ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { tc.serve(w) }) // reconnects get the same
			err := fa.agentWith(t, quick).Send(t.Context(), message("hi"), nil)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrNotSent) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSendReturnsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		sse(t, w, "livestream/01-slow-stream.response.sse", 4)
		cancel()
		<-call.Done
	})
	err := fa.agent(t).Send(ctx, message("hi"), nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStreamLost) {
		t.Errorf("err = %v, want context.Canceled and not ErrStreamLost", err)
	}
}
