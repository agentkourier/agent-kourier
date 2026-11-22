// Package anthropic adapts the Anthropic Messages API to Google ADK's model interface, as package bedrock adapts Amazon
// Bedrock's Converse: text, function tools (ask_user among them), and the thinking blocks of a model that thinks, which
// go back to the API unchanged with the rest of the turn.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/toolschema"
)

// DefaultMaxTokens is the most tokens a reply may have, its thinking included, when the request names no limit, which the
// agent's never does: the Messages API requires one. A reply that reaches it ends with what was written so far.
const DefaultMaxTokens = 8192

// MaxRetries is how many times the SDK sends a request again after a connection error or a status it may retry (408,
// 409, 429 and 5xx), before the call fails. An error event in a stream that has opened is not retried.
const MaxRetries = 2

// Client is the SDK's Messages service, as far as the model uses it, so that Observe and a test can stand in for it.
type Client interface {
	NewStreaming(ctx context.Context, params sdk.MessageNewParams, opts ...option.RequestOption) *ssestream.Stream[sdk.MessageStreamEventUnion]
}

// NewClient returns the SDK's Messages service as the SDK's environment configures it (the credentials, ANTHROPIC_API_KEY
// first, and ANTHROPIC_BASE_URL), retrying MaxRetries times, then as opts say.
func NewClient(opts ...option.RequestOption) Client {
	c := sdk.NewClient(append([]option.RequestOption{option.WithMaxRetries(MaxRetries)}, opts...)...)
	return &c.Messages
}

// Model implements model.LLM for text and function-tool conversations.
type Model struct {
	// ID is the model, such as claude-haiku-4-5.
	ID     string
	Client Client
}

var _ model.LLM = (*Model)(nil)

// Name returns the model's ID.
func (m *Model) Name() string { return m.ID }

// GenerateContent sends one model turn. It always streams from the API, so that a long reply cannot outlast an unstreamed
// request; with stream set it yields each text delta as a partial response, and in either case one complete response.
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params, err := m.params(req)
		if err != nil {
			yield(nil, err)
			return
		}
		s := m.Client.NewStreaming(ctx, params)
		defer func() { _ = s.Close() }()
		var msg sdk.Message
		stopped := false
		for s.Next() {
			event := s.Current()
			if err := msg.Accumulate(event); err != nil {
				yield(nil, errors.New("anthropic stream is out of order"))
				return
			}
			switch event.Type {
			case "content_block_delta":
				if stream && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					partial := &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: event.Delta.Text}}}, Partial: true}
					if !yield(partial, nil) {
						return
					}
				}
			case "message_stop":
				stopped = true
			}
		}
		if err := s.Err(); err != nil {
			yield(nil, modelError(ctx, err))
			return
		}
		if !stopped {
			yield(nil, errors.New("anthropic stream ended before the message stopped"))
			return
		}
		r, err := response(&msg)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(r, nil)
	}
}

// modelError is err without anything the API wrote in it, which may quote the request: the context's error, or the
// status and the type of the API's error.
func modelError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var api *sdk.Error
	if !errors.As(err, &api) {
		return errors.New("anthropic request failed")
	}
	kind := string(api.Type())
	if api.StatusCode == http.StatusOK { // an error event in a stream that had opened
		if kind == "" {
			return errors.New("anthropic stream failed")
		}
		return errors.New("anthropic stream failed: " + kind)
	}
	msg := "anthropic request failed: " + strconv.Itoa(api.StatusCode)
	if kind != "" {
		msg += " " + kind
	}
	return errors.New(msg)
}

// response is the complete response of msg. Its thinking blocks ride on the part that follows them, or on the last part
// when none does.
func response(msg *sdk.Message) (*model.LLMResponse, error) {
	if n := len(msg.Content); n > 0 && msg.Content[n-1].Type == "tool_use" && finishReason(msg.StopReason) == genai.FinishReasonMaxTokens {
		// The SDK empties a call's input that the limit cut off, which would read as a call with no arguments.
		return nil, fmt.Errorf("anthropic reply was cut off in a tool call (stop reason %s)", msg.StopReason)
	}
	content := &genai.Content{Role: "model"}
	var pending []thought
	for _, b := range msg.Content {
		var part *genai.Part
		switch b.Type {
		case "thinking":
			pending = append(pending, thought{Type: b.Type, Thinking: b.Thinking, Signature: b.Signature})
			continue
		case "redacted_thinking":
			pending = append(pending, thought{Type: b.Type, Data: b.Data})
			continue
		case "text":
			if b.Text == "" {
				continue
			}
			part = &genai.Part{Text: b.Text}
		case "tool_use":
			var args map[string]any
			if json.Unmarshal(b.Input, &args) != nil {
				return nil, fmt.Errorf("anthropic returned invalid tool arguments (stop reason %s)", msg.StopReason)
			}
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: b.ID, Name: b.Name, Args: args}}
		default:
			return nil, fmt.Errorf("anthropic returned a %s block, which the add-on does not support", b.Type)
		}
		if len(pending) > 0 {
			part.ThoughtSignature = thoughts{Before: pending}.encode()
			pending = nil
		}
		content.Parts = append(content.Parts, part)
	}
	if n := len(content.Parts); n > 0 && len(pending) > 0 {
		last := content.Parts[n-1]
		t := decodeThoughts(last.ThoughtSignature)
		t.After = pending
		last.ThoughtSignature = t.encode()
	}
	r := &model.LLMResponse{Content: content, TurnComplete: true, FinishReason: finishReason(msg.StopReason)}
	u := msg.Usage
	in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     tokens(in),
		CandidatesTokenCount: tokens(u.OutputTokens),
		TotalTokenCount:      tokens(in + u.OutputTokens),
	}
	if len(content.Parts) == 0 && r.FinishReason != genai.FinishReasonStop {
		// Nothing to show: ADK reads an error code as a failed turn.
		r.ErrorCode = string(r.FinishReason)
		r.ErrorMessage = fmt.Sprintf("the model stopped (%s) without a reply", msg.StopReason)
	}
	return r, nil
}

// tokens narrows a count of the API to genai's width, holding one past it at
// the largest value genai can carry.
func tokens(n int64) int32 {
	return int32(min(max(n, 0), math.MaxInt32))
}

// finishReason is genai's name for how the model stopped.
func finishReason(reason sdk.StopReason) genai.FinishReason {
	switch reason {
	case sdk.StopReasonEndTurn, sdk.StopReasonToolUse, sdk.StopReasonStopSequence:
		return genai.FinishReasonStop
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		return genai.FinishReasonMaxTokens
	case sdk.StopReasonRefusal:
		return genai.FinishReasonSafety
	}
	return genai.FinishReasonOther
}

// thoughts are the thinking blocks of a reply, kept on a part's ThoughtSignature, which ADK stores with the part in the
// session, so that they go back to the API unchanged and in their place, as it requires of a turn that called a tool:
// Before the part, and After it (only the reply's last part, for blocks that end the reply).
type thoughts struct {
	Before []thought `json:"before,omitempty"`
	After  []thought `json:"after,omitempty"`
}

// thought is a thinking or redacted_thinking block.
type thought struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// thoughtsPrefix marks a ThoughtSignature as this package's: another provider's is not sent.
const thoughtsPrefix = "agentkourier.anthropic.thoughts/v1:"

func (t thoughts) encode() []byte {
	raw, _ := json.Marshal(t) // strings alone: it cannot fail
	return append([]byte(thoughtsPrefix), raw...)
}

// decodeThoughts reads sig, or returns none when it is not this package's.
func decodeThoughts(sig []byte) thoughts {
	var t thoughts
	raw, ok := strings.CutPrefix(string(sig), thoughtsPrefix)
	if !ok || json.Unmarshal([]byte(raw), &t) != nil {
		return thoughts{}
	}
	return t
}

func (t thought) block() sdk.ContentBlockParamUnion {
	if t.Type == "redacted_thinking" {
		return sdk.NewRedactedThinkingBlock(t.Data)
	}
	return sdk.NewThinkingBlock(t.Signature, t.Thinking)
}

func blocksOf(ts []thought) []sdk.ContentBlockParamUnion {
	out := make([]sdk.ContentBlockParamUnion, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.block())
	}
	return out
}

// params is the Messages request of req.
func (m *Model) params(req *model.LLMRequest) (sdk.MessageNewParams, error) {
	if req == nil || m.ID == "" || m.Client == nil {
		return sdk.MessageNewParams{}, errors.New("anthropic model and request are required")
	}
	p := sdk.MessageNewParams{Model: sdk.Model(m.ID), MaxTokens: DefaultMaxTokens}
	type message struct {
		assistant bool
		blocks    []sdk.ContentBlockParamUnion
	}
	var messages []message
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		assistant := c.Role == "model" || c.Role == "assistant"
		blocks, err := toBlocks(c.Parts)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(messages); n > 0 && messages[n-1].assistant == assistant {
			messages[n-1].blocks = append(messages[n-1].blocks, blocks...)
		} else {
			messages = append(messages, message{assistant: assistant, blocks: blocks})
		}
	}
	for _, m := range messages {
		if m.assistant {
			p.Messages = append(p.Messages, sdk.NewAssistantMessage(m.blocks...))
		} else {
			p.Messages = append(p.Messages, sdk.NewUserMessage(resultsFirst(m.blocks)...))
		}
	}
	c := req.Config
	if c == nil {
		return p, nil
	}
	switch {
	case c.Temperature != nil:
		return sdk.MessageNewParams{}, errors.New("anthropic model sends no temperature")
	case c.TopP != nil:
		return sdk.MessageNewParams{}, errors.New("anthropic model sends no top_p")
	case c.TopK != nil:
		return sdk.MessageNewParams{}, errors.New("anthropic model sends no top_k")
	}
	if c.SystemInstruction != nil {
		for _, part := range c.SystemInstruction.Parts {
			if part != nil && part.Text != "" {
				p.System = append(p.System, sdk.TextBlockParam{Text: part.Text})
			}
		}
	}
	if c.MaxOutputTokens > 0 {
		p.MaxTokens = int64(c.MaxOutputTokens)
	}
	p.StopSequences = c.StopSequences
	for _, t := range c.Tools {
		if t == nil {
			continue
		}
		for _, f := range t.FunctionDeclarations {
			if f == nil {
				continue
			}
			tool, err := toolOf(f)
			if err != nil {
				return sdk.MessageNewParams{}, err
			}
			p.Tools = append(p.Tools, tool)
		}
	}
	return p, nil
}

// toBlocks is the content blocks of parts: text, a function call as tool_use and its response as tool_result, each after
// the thinking blocks it carries.
func toBlocks(parts []*genai.Part) ([]sdk.ContentBlockParamUnion, error) {
	var blocks []sdk.ContentBlockParamUnion
	for _, part := range parts {
		if part == nil {
			continue
		}
		if unsupported(part) {
			return nil, errors.New("anthropic model supports text and function tools only")
		}
		t := decodeThoughts(part.ThoughtSignature)
		blocks = append(blocks, blocksOf(t.Before)...)
		switch {
		case part.FunctionCall != nil:
			f := part.FunctionCall
			var input any = f.Args
			if f.Args == nil {
				input = map[string]any{} // the API requires an object
			}
			blocks = append(blocks, sdk.NewToolUseBlock(f.ID, input, f.Name))
		case part.FunctionResponse != nil:
			f := part.FunctionResponse
			raw, err := json.Marshal(f.Response)
			if err != nil {
				return nil, errors.New("invalid function response")
			}
			blocks = append(blocks, sdk.ContentBlockParamUnion{OfToolResult: &sdk.ToolResultBlockParam{ToolUseID: f.ID,
				Content: []sdk.ToolResultBlockParamContentUnion{{OfText: &sdk.TextBlockParam{Text: string(raw)}}}}})
		case part.Thought:
			// Reasoning from elsewhere: not words the model said, and nothing the API takes back.
		case part.Text != "":
			blocks = append(blocks, sdk.NewTextBlock(part.Text))
		}
		blocks = append(blocks, blocksOf(t.After)...)
	}
	return blocks, nil
}

// unsupported reports whether part holds anything but text, a thought marker and signature, a function call or a
// function response. It is stated as what is left over, so that a field a later genai adds is refused, not lost.
func unsupported(part *genai.Part) bool {
	rest := *part
	rest.Text, rest.Thought, rest.ThoughtSignature, rest.FunctionCall, rest.FunctionResponse = "", false, nil, nil, nil
	return !reflect.ValueOf(rest).IsZero()
}

// resultsFirst puts the tool_result blocks of a user message before its other blocks, keeping each kind's order: the API
// takes tool results first.
func resultsFirst(blocks []sdk.ContentBlockParamUnion) []sdk.ContentBlockParamUnion {
	out := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		if b.OfToolResult != nil {
			out = append(out, b)
		}
	}
	for _, b := range blocks {
		if b.OfToolResult == nil {
			out = append(out, b)
		}
	}
	return out
}

// toolOf declares f as a tool, with its parameters' JSON Schema, which must be an object.
func toolOf(f *genai.FunctionDeclaration) (sdk.ToolUnionParam, error) {
	schema, err := toolschema.Of(f)
	if err != nil {
		return sdk.ToolUnionParam{}, err
	}
	if t, ok := schema["type"]; ok && t != "object" {
		return sdk.ToolUnionParam{}, fmt.Errorf("the parameters of the tool %s are not an object", f.Name)
	}
	in := sdk.ToolInputSchemaParam{Properties: schema["properties"]}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				in.Required = append(in.Required, s)
			}
		}
	}
	for k, v := range schema {
		switch k {
		case "type", "properties", "required":
		default:
			if in.ExtraFields == nil {
				in.ExtraFields = map[string]any{}
			}
			in.ExtraFields[k] = v
		}
	}
	tool := sdk.ToolParam{Name: f.Name, InputSchema: in}
	if f.Description != "" {
		tool.Description = sdk.String(f.Description)
	}
	return sdk.ToolUnionParam{OfTool: &tool}, nil
}
