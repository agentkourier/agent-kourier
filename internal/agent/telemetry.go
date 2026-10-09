package agent

import (
	"context"
	"errors"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func finishAgent(op *telemetry.Operation, err error) {
	switch {
	case errors.Is(err, ErrBusy):
		op.FinishOutcome(nil, "busy")
	case errors.Is(err, a2a.ErrTaskNotFound):
		op.FinishOutcome(nil, "not_found")
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		op.FinishOutcome(nil, "already_ended")
	case errors.Is(err, ErrCredentialsRejected):
		op.FinishOutcome(err, "credentials_rejected")
	case errors.Is(err, ErrForbidden):
		op.FinishOutcome(err, "forbidden")
	case errors.Is(err, ErrOutcomeUnknown):
		op.FinishOutcome(err, "outcome_unknown")
	case errors.Is(err, ErrStreamLost):
		op.FinishOutcome(err, "stream_lost")
	case errors.Is(err, ErrAnswerRejected):
		op.FinishOutcome(err, "answer_rejected")
	default:
		op.Finish(err)
	}
}

func observeHandler(ctx context.Context, operation string, h Handler, final *a2a.TaskState) Handler {
	started := time.Now()
	texted := false
	return func(ev Event) {
		switch e := ev.(type) {
		case StateEvent:
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("agentkourier.task.id", string(e.TaskID)), attribute.String("agentkourier.context.id", e.ContextID), attribute.String("agentkourier.task.state", string(e.State)))
		case TextEvent:
			if operation != "follow" && !texted && e.Text != "" {
				texted = true
				telemetry.Observe(ctx, "agentkourier_agent_first_text_duration_seconds", time.Since(started), operation)
			}
		case FinalEvent:
			if e.Task != nil {
				*final = e.Task.Status.State
				trace.SpanFromContext(ctx).SetAttributes(attribute.String("agentkourier.task.state", string(e.Task.Status.State)))
				if e.Task.Status.State == a2a.TaskStateFailed {
					trace.SpanFromContext(ctx).SetStatus(codes.Error, "task_failed")
				}
			}
		}
		if h != nil {
			h(ev)
		}
	}
}

func finishAgentTask(op *telemetry.Operation, err error, state a2a.TaskState) {
	if err != nil || state == "" {
		finishAgent(op, err)
		return
	}
	switch state {
	case a2a.TaskStateFailed, a2a.TaskStateRejected:
		op.FinishOutcome(errors.New("task ended without success"), string(state))
	case a2a.TaskStateCompleted, a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired, a2a.TaskStateCanceled:
		op.FinishOutcome(nil, string(state))
	default:
		op.FinishOutcome(errors.New("unrecognized final task state"), "unknown_state")
	}
}
