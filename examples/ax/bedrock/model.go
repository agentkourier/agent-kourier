// Package bedrock adapts Amazon Bedrock Converse to Google ADK's model interface.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	bt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Client is the AWS SDK contract, allowing deterministic model tests.
type Client interface {
	Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// Model implements model.LLM for text and function-tool conversations.
type Model struct {
	ID     string
	Client Client
}

var _ model.LLM = (*Model)(nil)

// Name returns the configured model or inference profile ID.
func (m *Model) Name() string { return m.ID }

// GenerateContent sends one model turn, yielding text deltas and one complete response.
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		input, err := m.request(req)
		if err != nil {
			yield(nil, err)
			return
		}
		if !stream {
			out, err := m.Client.Converse(ctx, input)
			if err != nil {
				yield(nil, modelError(ctx, err))
				return
			}
			msg, ok := out.Output.(*bt.ConverseOutputMemberMessage)
			if !ok {
				yield(nil, errors.New("bedrock returned no message"))
				return
			}
			content, err := fromBlocks(msg.Value.Content)
			if err != nil {
				yield(nil, err)
				return
			}
			yield(response(content, out.Usage), nil)
			return
		}
		out, err := m.Client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{ModelId: input.ModelId, Messages: input.Messages, System: input.System, InferenceConfig: input.InferenceConfig, ToolConfig: input.ToolConfig})
		if err != nil {
			yield(nil, modelError(ctx, err))
			return
		}
		events := out.GetStream()
		defer func() { _ = events.Close() }()
		blocks := map[int32]*streamBlock{}
		var usage *bt.TokenUsage
		stopped := false
		for event := range events.Events() {
			switch e := event.(type) {
			case *bt.ConverseStreamOutputMemberContentBlockStart:
				if start, ok := e.Value.Start.(*bt.ContentBlockStartMemberToolUse); ok {
					blocks[aws.ToInt32(e.Value.ContentBlockIndex)] = &streamBlock{id: aws.ToString(start.Value.ToolUseId), name: aws.ToString(start.Value.Name)}
				}
			case *bt.ConverseStreamOutputMemberContentBlockDelta:
				index := aws.ToInt32(e.Value.ContentBlockIndex)
				block := blocks[index]
				if block == nil {
					block = &streamBlock{}
					blocks[index] = block
				}
				switch delta := e.Value.Delta.(type) {
				case *bt.ContentBlockDeltaMemberText:
					block.text += delta.Value
					if !yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: delta.Value}}}, Partial: true}, nil) {
						return
					}
				case *bt.ContentBlockDeltaMemberToolUse:
					block.input += aws.ToString(delta.Value.Input)
				default:
					yield(nil, errors.New("unsupported Bedrock stream content"))
					return
				}
			case *bt.ConverseStreamOutputMemberMessageStop:
				stopped = true
			case *bt.ConverseStreamOutputMemberMetadata:
				usage = e.Value.Usage
			}
		}
		if events.Err() != nil {
			yield(nil, modelError(ctx, events.Err()))
			return
		}
		if !stopped {
			yield(nil, errors.New("bedrock stream ended before message completion"))
			return
		}
		indices := make([]int32, 0, len(blocks))
		for index := range blocks {
			indices = append(indices, index)
		}
		slices.Sort(indices)
		content := &genai.Content{Role: "model"}
		for _, index := range indices {
			b := blocks[index]
			if b.name != "" {
				var args map[string]any
				if json.Unmarshal([]byte(b.input), &args) != nil {
					yield(nil, errors.New("invalid Bedrock tool arguments"))
					return
				}
				content.Parts = append(content.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: b.id, Name: b.name, Args: args}})
			} else {
				content.Parts = append(content.Parts, &genai.Part{Text: b.text})
			}
		}
		yield(response(content, usage), nil)
	}
}

type streamBlock struct{ id, name, text, input string }

func modelError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		if api.ErrorCode() == "ValidationException" {
			message := strings.ToLower(api.ErrorMessage())
			for _, field := range []string{"temperature", "top_p", "topp", "maxtokens", "max_tokens", "schema", "inference profile", "on-demand", "model identifier", "required", "tool"} {
				if strings.Contains(message, field) {
					return errors.New("bedrock validation failed for " + field)
				}
			}
		}
		switch api.ErrorCode() {
		case "AccessDeniedException", "ValidationException", "ThrottlingException", "ResourceNotFoundException", "ServiceUnavailableException", "ExpiredTokenException", "UnrecognizedClientException":
			return errors.New("bedrock request failed: " + api.ErrorCode())
		}
	}
	return errors.New("bedrock request failed")
}
func response(content *genai.Content, usage *bt.TokenUsage) *model.LLMResponse {
	r := &model.LLMResponse{Content: content, TurnComplete: true, FinishReason: genai.FinishReasonStop}
	if usage != nil {
		r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: aws.ToInt32(usage.InputTokens), CandidatesTokenCount: aws.ToInt32(usage.OutputTokens), TotalTokenCount: aws.ToInt32(usage.TotalTokens)}
	}
	return r
}

func (m *Model) request(req *model.LLMRequest) (*bedrockruntime.ConverseInput, error) {
	if req == nil || m.ID == "" || m.Client == nil {
		return nil, errors.New("bedrock model and request are required")
	}
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(m.ID)}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		role := bt.ConversationRoleUser
		if c.Role == "model" || c.Role == "assistant" {
			role = bt.ConversationRoleAssistant
		}
		msg := bt.Message{Role: role}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil:
				f := p.FunctionCall
				msg.Content = append(msg.Content, &bt.ContentBlockMemberToolUse{Value: bt.ToolUseBlock{ToolUseId: aws.String(f.ID), Name: aws.String(f.Name), Input: document.NewLazyDocument(f.Args)}})
			case p.FunctionResponse != nil:
				f := p.FunctionResponse
				msg.Content = append(msg.Content, &bt.ContentBlockMemberToolResult{Value: bt.ToolResultBlock{ToolUseId: aws.String(f.ID), Content: []bt.ToolResultContentBlock{&bt.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(f.Response)}}}})
			case p.Text != "":
				msg.Content = append(msg.Content, &bt.ContentBlockMemberText{Value: p.Text})
			default:
				return nil, errors.New("bedrock example supports text and function tools only")
			}
		}
		if len(msg.Content) == 0 {
			continue
		}
		if n := len(in.Messages); n > 0 && in.Messages[n-1].Role == role {
			in.Messages[n-1].Content = append(in.Messages[n-1].Content, msg.Content...)
		} else {
			in.Messages = append(in.Messages, msg)
		}
	}
	if req.Config != nil {
		c := req.Config
		if c.SystemInstruction != nil {
			for _, p := range c.SystemInstruction.Parts {
				if p != nil && p.Text != "" {
					in.System = append(in.System, &bt.SystemContentBlockMemberText{Value: p.Text})
				}
			}
		}
		in.InferenceConfig = &bt.InferenceConfiguration{Temperature: c.Temperature, TopP: c.TopP, StopSequences: c.StopSequences}
		if c.MaxOutputTokens > 0 {
			in.InferenceConfig.MaxTokens = aws.Int32(c.MaxOutputTokens)
		}
		for _, t := range c.Tools {
			if t == nil {
				continue
			}
			for _, f := range t.FunctionDeclarations {
				if f == nil {
					continue
				}
				schema := f.ParametersJsonSchema
				if schema == nil && f.Parameters != nil {
					schema = f.Parameters
				}
				if schema != nil {
					// ADK's schema structs have a custom JSON marshaler. Smithy
					// documents require normalized JSON values to preserve that schema.
					raw, err := json.Marshal(schema)
					if err != nil {
						return nil, errors.New("invalid tool schema")
					}
					var mapped any
					if json.Unmarshal(raw, &mapped) != nil {
						return nil, errors.New("invalid tool schema")
					}
					lowerTypes(mapped)
					schema = mapped
				}
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				if in.ToolConfig == nil {
					in.ToolConfig = &bt.ToolConfiguration{}
				}
				in.ToolConfig.Tools = append(in.ToolConfig.Tools, &bt.ToolMemberToolSpec{Value: bt.ToolSpecification{Name: aws.String(f.Name), Description: aws.String(f.Description), InputSchema: &bt.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)}}})
			}
		}
	}
	return in, nil
}
func lowerTypes(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "type" {
				if t, ok := val.(string); ok {
					x[k] = strings.ToLower(t)
				}
			} else {
				lowerTypes(val)
			}
		}
	case []any:
		for _, val := range x {
			lowerTypes(val)
		}
	}
}
func fromBlocks(blocks []bt.ContentBlock) (*genai.Content, error) {
	c := &genai.Content{Role: "model"}
	for _, block := range blocks {
		switch b := block.(type) {
		case *bt.ContentBlockMemberText:
			c.Parts = append(c.Parts, &genai.Part{Text: b.Value})
		case *bt.ContentBlockMemberToolUse:
			var args map[string]any
			if b.Value.Input.UnmarshalSmithyDocument(&args) != nil {
				return nil, errors.New("invalid Bedrock tool arguments")
			}
			c.Parts = append(c.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: aws.ToString(b.Value.ToolUseId), Name: aws.ToString(b.Value.Name), Args: args}})
		default:
			return nil, errors.New("unsupported Bedrock response content")
		}
	}
	return c, nil
}
