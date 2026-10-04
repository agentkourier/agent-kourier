package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/auth/bearer"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestConverseStreamTextAndToolWithBearer(t *testing.T) {
	for _, tool := range []bool{false, true} {
		t.Run(map[bool]string{true: "tool", false: "text"}[tool], func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("missing bearer authentication")
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if len(req["messages"].([]any)) != 1 {
					t.Error("wrong request")
				}
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				enc := eventstream.NewEncoder()
				emit := func(kind, payload string) {
					var headers eventstream.Headers
					headers.Set(":message-type", eventstream.StringValue("event"))
					headers.Set(":event-type", eventstream.StringValue(kind))
					headers.Set(":content-type", eventstream.StringValue("application/json"))
					if err := enc.Encode(w, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
						t.Error(err)
					}
				}
				emit("messageStart", `{"role":"assistant"}`)
				if tool {
					emit("contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"call-1","name":"ask_user"}}}`)
					emit("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"question\":\"Which?\"}"}}}`)
				} else {
					emit("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hello "}}`)
					emit("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"world"}}`)
				}
				emit("contentBlockStop", `{"contentBlockIndex":0}`)
				emit("messageStop", `{"stopReason":"end_turn"}`)
				emit("metadata", `{"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12},"metrics":{"latencyMs":1}}`)
			}))
			defer srv.Close()
			client := bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(), BearerAuthTokenProvider: bearer.StaticTokenProvider{Token: bearer.Token{Value: "test-key"}}})
			m := &Model{ID: "us.anthropic.claude-haiku-4-5-20251001-v1:0", Client: client}
			var partial strings.Builder
			var final *model.LLMResponse
			for out, err := range m.GenerateContent(t.Context(), &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}, true) {
				if err != nil {
					t.Fatal(err)
				}
				if out.Partial {
					partial.WriteString(out.Content.Parts[0].Text)
				} else {
					final = out
				}
			}
			if final == nil || !final.TurnComplete || final.UsageMetadata.TotalTokenCount != 12 {
				t.Fatalf("missing completion: %+v", final)
			}
			if tool {
				if final.Content.Parts[0].FunctionCall.Args["question"] != "Which?" {
					t.Fatal("tool arguments lost")
				}
			} else if final.Content.Parts[0].Text != "Hello world" || partial.String() != "Hello world" {
				t.Fatal("stream text lost")
			}
		})
	}
}

func TestConverseConvertsToolHistoryAndSchemas(t *testing.T) {
	m := &Model{ID: "haiku", Client: bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1"})}
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "ask_user", Args: map[string]any{"question": "Which?"}}}}}, {Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "ask_user", Response: map[string]any{"answer": "checkout"}}}}}}, Config: &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "instructions"}}}, Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "ask_user", Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"question": {Type: genai.TypeString}}}}}}}}}
	input, err := m.request(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Messages) != 2 || len(input.System) != 1 || len(input.ToolConfig.Tools) != 1 {
		t.Fatalf("wrong converted request: %+v", input)
	}
}

func TestModelErrorsDoNotEchoSecrets(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "test-key supersecret", http.StatusUnauthorized)
	}))
	defer srv.Close()
	client := bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(), BearerAuthTokenProvider: bearer.StaticTokenProvider{Token: bearer.Token{Value: "test-key"}}})
	m := &Model{ID: "haiku", Client: client}
	for _, err := range m.GenerateContent(context.Background(), &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}, false) {
		if err == nil || strings.Contains(err.Error(), "test-key") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(modelError(ctx, nil), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestADKJSONSchemaSurvivesSmithyEncoding(t *testing.T) {
	m := &Model{ID: "haiku", Client: bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1"})}
	schema := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"question": {Type: "string"}}, Required: []string{"question"}}
	in, err := m.request(&model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "ask_user", ParametersJsonSchema: schema}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	spec := in.ToolConfig.Tools[0].(*bt.ToolMemberToolSpec)
	encoded, err := spec.Value.InputSchema.(*bt.ToolInputSchemaMemberJson).Value.MarshalSmithyDocument()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "object" || got["properties"].(map[string]any)["question"].(map[string]any)["type"] != "string" {
		t.Fatalf("invalid schema: %s", encoded)
	}
}
