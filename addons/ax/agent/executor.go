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

	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// TextExecutor exposes ask_user as a plain-text A2A question while keeping its ADK payload durable.
type TextExecutor struct {
	Inner a2asrv.AgentExecutor
}

var (
	_ a2asrv.AgentExecutor    = (*TextExecutor)(nil)
	_ runtime.ResumeValidator = (*TextExecutor)(nil)
)

// Execute bridges only ask_user; it never treats plain text as tool authorization. The kit's runtime counts and traces
// each call as one turn.
func (e *TextExecutor) Execute(ctx context.Context, req *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { e.execute(ctx, req, yield) }
}

// ValidateResume accepts an answer to an ask_user question that is text, not blank: the one Execute can pass to ADK.
func (e *TextExecutor) ValidateResume(_ context.Context, _ *a2a.Task, msg *a2a.Message) error {
	_, err := answerText(msg)
	return err
}

// answerText is the text of an answer, its text parts joined, or a2a.ErrInvalidParams.
func answerText(msg *a2a.Message) (string, error) {
	var answer []string
	for _, part := range msg.Parts {
		if part == nil {
			return "", a2a.ErrInvalidParams
		}
		text, ok := part.Content.(a2a.Text)
		if !ok {
			return "", a2a.ErrInvalidParams
		}
		answer = append(answer, string(text))
	}
	text := strings.TrimSpace(strings.Join(answer, "\n"))
	if text == "" {
		return "", a2a.ErrInvalidParams
	}
	return text, nil
}

// execute runs one turn, yielding its events.
func (e *TextExecutor) execute(ctx context.Context, req *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	if task := req.StoredTask; task != nil && task.Status.State == a2a.TaskStateInputRequired {
		call, err := pendingCall(task)
		if err != nil {
			yield(nil, err)
			return
		}
		text, err := answerText(req.Message)
		if err != nil {
			yield(nil, err)
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
