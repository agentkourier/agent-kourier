package agent

import (
	"context"
	"errors"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// TextExecutor exposes ask_user as a plain-text A2A question while keeping its ADK payload durable.
type TextExecutor struct {
	Inner a2asrv.AgentExecutor
	// Telemetry counts each turn by how it ended and traces it (agentkourier.ax.turn). Nil records nothing.
	Telemetry *telemetry.Telemetry
}

var _ a2asrv.AgentExecutor = (*TextExecutor)(nil)

// Execute bridges only ask_user; it never treats plain text as tool authorization. Each call is one turn, which it
// traces as a child of the span in ctx (the caller's) and counts when it returns.
func (e *TextExecutor) Execute(ctx context.Context, req *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, turn := e.Telemetry.StartTurn(ctx, string(req.TaskID), req.ContextID)
		o := &outcome{}
		defer func() { turn.End(o.of(ctx)) }()
		e.execute(ctx, req, o.track(yield))
	}
}

// execute runs one turn, yielding its events.
func (e *TextExecutor) execute(ctx context.Context, req *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	if task := req.StoredTask; task != nil && task.Status.State == a2a.TaskStateInputRequired {
		call, err := pendingCall(task)
		if err != nil {
			yield(nil, err)
			return
		}
		var answer []string
		for _, part := range req.Message.Parts {
			if part == nil {
				yield(nil, a2a.ErrInvalidParams)
				return
			}
			text, ok := part.Content.(a2a.Text)
			if !ok {
				yield(nil, a2a.ErrInvalidParams)
				return
			}
			answer = append(answer, string(text))
		}
		text := strings.TrimSpace(strings.Join(answer, "\n"))
		if text == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		copyReq := *req
		copyMsg := *req.Message
		copyTask := *task
		copyStatus := task.Status
		copyMeta := task.Metadata
		responsePart, err := adka2a.ToA2APart(&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"answer": text}}}, nil)
		if err != nil {
			yield(nil, err)
			return
		}
		copyMsg.Parts = []*a2a.Part{responsePart}
		// ADK validates the original function call, not the text displayed to Agent Kourier.
		callPart, err := adka2a.ToA2APart(&genai.Part{FunctionCall: call}, []string{call.ID})
		if err != nil {
			yield(nil, err)
			return
		}
		copyStatus.Message = a2a.NewMessage(a2a.MessageRoleAgent, callPart)
		copyTask.Status = copyStatus
		copyTask.Metadata = copyMeta
		copyReq.StoredTask = &copyTask
		copyReq.Message = &copyMsg
		req = &copyReq
	}
	for event, err := range e.Inner.Execute(ctx, req) {
		if err != nil {
			yield(nil, errors.New("agent execution failed"))
			return
		}
		if status, ok := event.(*a2a.TaskStatusUpdateEvent); ok && status.Status.State == a2a.TaskStateInputRequired {
			if status.Status.Message == nil {
				yield(nil, errors.New("missing question"))
				return
			}
			parts, err := adka2a.ToGenAIParts(status.Status.Message.Parts)
			if err != nil {
				yield(nil, errors.New("invalid question"))
				return
			}
			var call *genai.FunctionCall
			for _, part := range parts {
				if part.FunctionCall != nil {
					if call != nil || part.FunctionCall.Name != "ask_user" {
						yield(nil, errors.New("unsupported interaction"))
						return
					}
					call = part.FunctionCall
				}
			}
			if call == nil {
				yield(nil, errors.New("missing question call"))
				return
			}
			question, ok := call.Args["question"].(string)
			if !ok || strings.TrimSpace(question) == "" {
				yield(nil, errors.New("missing question text"))
				return
			}
			status.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(question))
			if status.Metadata == nil {
				status.Metadata = map[string]any{}
			}
			status.Metadata["agentkourier.dev/ask-user"] = map[string]any{"id": call.ID, "name": call.Name, "args": call.Args}
		}
		if !yield(event, nil) {
			return
		}
	}
}

// outcome follows what a turn yields, to say how it ended.
type outcome struct {
	state   a2a.TaskState
	errored bool // the turn ended on an error
	stopped bool // the consumer stopped taking events
}

// track returns yield, noting each state and error it passes and whether the consumer stopped.
func (o *outcome) track(yield func(a2a.Event, error) bool) func(a2a.Event, error) bool {
	return func(ev a2a.Event, err error) bool {
		if err != nil {
			o.errored = true
		}
		switch v := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			o.state = v.Status.State
		case *a2a.Task:
			o.state = v.Status.State
		}
		if !yield(ev, err) {
			o.stopped = true
			return false
		}
		return true
	}
}

// of is the turn's outcome once it has returned. One that ended on an error failed; one that stopped before it settled
// or paused was canceled.
func (o *outcome) of(ctx context.Context) telemetry.TurnOutcome {
	switch {
	case o.state == a2a.TaskStateCanceled:
		return telemetry.TurnCanceled
	case o.state == a2a.TaskStateCompleted && !o.errored:
		return telemetry.TurnCompleted
	case o.state == a2a.TaskStateInputRequired && !o.errored:
		return telemetry.TurnInputRequired
	case o.state == a2a.TaskStateFailed, o.errored:
		return telemetry.TurnFailed
	case o.stopped || ctx.Err() != nil:
		return telemetry.TurnCanceled
	}
	return telemetry.TurnFailed
}

func pendingCall(task *a2a.Task) (*genai.FunctionCall, error) {
	data, ok := task.Metadata["agentkourier.dev/ask-user"].(map[string]any)
	if !ok {
		return nil, errors.New("missing persisted question")
	}
	id, _ := data["id"].(string)
	name, _ := data["name"].(string)
	args, _ := data["args"].(map[string]any)
	if id == "" || name != "ask_user" {
		return nil, errors.New("unsupported persisted question")
	}
	return &genai.FunctionCall{ID: id, Name: name, Args: args}, nil
}

// Cancel delegates task cancellation to ADK.
func (e *TextExecutor) Cancel(ctx context.Context, req *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return e.Inner.Cancel(ctx, req)
}
