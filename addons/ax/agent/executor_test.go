package agent

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2alog "github.com/a2aproject/a2a-go/v2/log"
	"google.golang.org/adk/v2/session"

	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// plainInner is an inner executor with no Cleanup.
type plainInner struct{}

func (plainInner) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func (plainInner) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

// cleaningInner is an inner executor with a Cleanup, which it notes.
type cleaningInner struct {
	plainInner
	calls  int
	ctx    context.Context
	ec     *a2asrv.ExecutorContext
	result a2a.SendMessageResult
	err    error
}

func (c *cleaningInner) Cleanup(ctx context.Context, ec *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	c.calls++
	c.ctx, c.ec, c.result, c.err = ctx, ec, result, err
}

type ctxKey struct{}

// TestCleanupReachesTheInnerExecutor holds the executor and the backend, as the kit finds the hook on them, to the
// inner executor's Cleanup: it runs once, with what a2a-go passed.
func TestCleanupReachesTheInnerExecutor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend func(a2asrv.AgentExecutor) runtime.Backend
	}{
		{"executor", func(inner a2asrv.AgentExecutor) runtime.Backend { return &TextExecutor{Inner: inner} }},
		{"backend", func(inner a2asrv.AgentExecutor) runtime.Backend {
			return &Backend{TextExecutor: &TextExecutor{Inner: inner}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &cleaningInner{}
			cleaner, ok := tc.backend(inner).(a2asrv.AgentExecutionCleaner)
			if !ok {
				t.Fatal("the backend has no Cleanup, so the kit never runs the inner executor's")
			}
			ctx := context.WithValue(t.Context(), ctxKey{}, "turn")
			ec := &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: "ctx-1"}
			result := &a2a.Task{ID: "task-1", Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
			cause := errors.New("agent execution failed")
			cleaner.Cleanup(ctx, ec, result, cause)
			if inner.calls != 1 {
				t.Fatalf("the inner Cleanup ran %d times, want 1", inner.calls)
			}
			//nolint:errorlint // the caller's error itself, not one that wraps it
			if inner.ctx.Value(ctxKey{}) != "turn" || inner.ec != ec || inner.result != a2a.SendMessageResult(result) || inner.err != cause {
				t.Errorf("the inner Cleanup was given (%v, %p, %v, %v), want the caller's (%p, %v, %v)",
					inner.ctx.Value(ctxKey{}), inner.ec, inner.result, inner.err, ec, result, cause)
			}
		})
	}
}

// TestCleanupWithoutAnInnerCleanupDoesNothing: an inner executor with no Cleanup leaves Cleanup a no-op.
func TestCleanupWithoutAnInnerCleanupDoesNothing(t *testing.T) {
	cleaner, ok := runtime.Backend(&TextExecutor{Inner: plainInner{}}).(a2asrv.AgentExecutionCleaner)
	if !ok {
		t.Fatal("the executor has no Cleanup")
	}
	cleaner.Cleanup(t.Context(), &a2asrv.ExecutorContext{TaskID: "task-1"}, nil, errors.New("failed"))
}

// TestTheADKExecutorsCleanupRuns runs the agent's own ADK executor's Cleanup through the executor: ADK logs the cause of
// a failed execution at WARN, to the logger in the context.
func TestTheADKExecutorsCleanupRuns(t *testing.T) {
	exec, err := NewTextExecutor(&fakeModel{}, session.InMemoryService())
	if err != nil {
		t.Fatal(err)
	}
	cleaner, ok := runtime.Backend(exec).(a2asrv.AgentExecutionCleaner)
	if !ok {
		t.Fatal("the executor has no Cleanup")
	}
	var logs bytes.Buffer
	ctx := a2alog.AttachLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)))
	ec := &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: "ctx-1", Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}
	cleaner.Cleanup(ctx, ec, nil, errors.New("the cause of the failure"))
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "the cause of the failure") {
		t.Errorf("ADK's Cleanup did not log the cause at WARN; logs: %q", out)
	}
}
