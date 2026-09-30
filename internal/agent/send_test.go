package agent

import (
	"context"
	"errors"
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

func TestLostStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serve    func(http.ResponseWriter)
		wantLost bool
	}{
		{"reset after the task began", func(w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", 4); drop() }, true},
		{"ended cleanly before the task settled", func(w http.ResponseWriter) { sse(t, w, "livestream/01-slow-stream.response.sse", 4) }, true},
		{"reset before any event", func(http.ResponseWriter) { drop() }, false},
		{"server error", func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusBadGateway) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { tc.serve(w) })
			err := fa.agent(t).Send(t.Context(), message("hi"), nil)
			if err == nil || errors.Is(err, ErrStreamLost) != tc.wantLost {
				t.Errorf("err = %v, want ErrStreamLost = %v (only once the agent has named a task)", err, tc.wantLost)
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

func TestSendFailsWhenThereIsNoConnection(t *testing.T) {
	boom := errors.New("no token")
	err := New(func(context.Context) (Conn, error) { return nil, boom }).Send(t.Context(), message("hi"), nil)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the source's error", err)
	}
}
