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
)

// TextExecutor exposes ask_user as a plain-text A2A question while keeping its ADK payload durable.
type TextExecutor struct{ Inner a2asrv.AgentExecutor }

var _ a2asrv.AgentExecutor = (*TextExecutor)(nil)

// Execute bridges only ask_user; it never treats plain text as tool authorization.
func (e *TextExecutor) Execute(ctx context.Context, req *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
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
			// ADK validates the original function call, not the text displayed to Courier.
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
				status.Metadata["agentcourier.dev/ask-user"] = map[string]any{"id": call.ID, "name": call.Name, "args": call.Args}
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}
func pendingCall(task *a2a.Task) (*genai.FunctionCall, error) {
	data, ok := task.Metadata["agentcourier.dev/ask-user"].(map[string]any)
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
