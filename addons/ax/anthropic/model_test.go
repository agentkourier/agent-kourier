package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// testKey is an obviously fake API key.
const testKey = "test-key-not-real"

// isolate unsets the SDK's environment, so that a developer's own Anthropic settings reach no test.
func isolate(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_PROFILE", "ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_IDENTITY_TOKEN"} {
		t.Setenv(name, "")
	}
}

// fakeAPI is the Messages API on a test server: each request is recorded, and answered by the script for its number (from 0).
type fakeAPI struct {
	t      *testing.T
	srv    *httptest.Server
	script func(w http.ResponseWriter, r *http.Request, n int)

	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
}

func newFakeAPI(t *testing.T, script func(w http.ResponseWriter, r *http.Request, n int)) *fakeAPI {
	t.Helper()
	isolate(t)
	f := &fakeAPI{t: t, script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("request %s %s; want POST /v1/messages", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		f.script(w, r, n)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// model is a Model on the fake API, through the client the agent builds.
func (f *fakeAPI) model() *Model {
	return &Model{ID: "claude-haiku-4-5", Client: NewClient(option.WithBaseURL(f.srv.URL), option.WithAPIKey(testKey))}
}

func (f *fakeAPI) request(n int) map[string]any {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		f.t.Fatalf("request %d of %d", n, len(f.requests))
	}
	return f.requests[n]
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// sse writes events, each "name data", as a stream of server-sent events.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		name, data, _ := strings.Cut(e, " ")
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// The events of a message, start to stop, with blocks between.
func start() string {
	return `message_start {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`
}

func stop(reason string, output int) []string {
	return []string{
		fmt.Sprintf(`message_delta {"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, reason, output),
		`message_stop {"type":"message_stop"}`,
	}
}

func textBlock(index int, deltas ...string) []string {
	out := []string{fmt.Sprintf(`content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)}
	for _, d := range deltas {
		raw, _ := json.Marshal(d)
		out = append(out, fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, raw))
	}
	return append(out, fmt.Sprintf(`content_block_stop {"type":"content_block_stop","index":%d}`, index))
}

func toolBlock(index int, id, name string, pieces ...string) []string {
	out := []string{fmt.Sprintf(`content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)}
	for _, p := range pieces {
		raw, _ := json.Marshal(p)
		out = append(out, fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, raw))
	}
	return append(out, fmt.Sprintf(`content_block_stop {"type":"content_block_stop","index":%d}`, index))
}

func thinkingBlock(index int, signature string) []string {
	return []string{
		fmt.Sprintf(`content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":"","signature":""}}`, index),
		fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":"weighing it"}}`, index),
		fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":%q}}`, index, signature),
		fmt.Sprintf(`content_block_stop {"type":"content_block_stop","index":%d}`, index),
	}
}

func redactedBlock(index int, data string) []string {
	return []string{
		fmt.Sprintf(`content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"redacted_thinking","data":%q}}`, index, data),
		fmt.Sprintf(`content_block_stop {"type":"content_block_stop","index":%d}`, index),
	}
}

func message(stopReason string, blocks ...[]string) []string {
	out := []string{start()}
	for _, b := range blocks {
		out = append(out, b...)
	}
	return append(out, stop(stopReason, 2)...)
}

func userText(text string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: text}}}}}
}

// content is the JSON of msg's content blocks.
func content(t *testing.T, msg sdk.MessageParam) string {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(m["content"])
}

// collect runs one model turn and returns its partial texts, its final response and its error.
func collect(ctx context.Context, m model.LLM, req *model.LLMRequest, stream bool) (partials []string, final *model.LLMResponse, err error) {
	for out, err := range m.GenerateContent(ctx, req, stream) {
		if err != nil {
			return partials, final, err
		}
		if out.Partial {
			partials = append(partials, out.Content.Parts[0].Text)
		} else {
			final = out
		}
	}
	return partials, final, nil
}

// A text answer streams each text delta as a partial response, then the whole message as one complete response; unstreamed, the
// same call yields the complete response alone. The request carries the key, the model, the default token limit, the system
// instruction and the conversation, and asks for a stream.
func TestStreamsTextThenTheCompleteMessage(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				sse(w, message("end_turn", textBlock(0, "Hello ", "world"))...)
			})
			req := userText("hello")
			req.Config = &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "Be brief."}}}}
			partials, final, err := collect(t.Context(), api.model(), req, stream)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{true: "Hello |world", false: ""}[stream]; strings.Join(partials, "|") != want {
				t.Errorf("partials %q; want %q", partials, want)
			}
			if final == nil || !final.TurnComplete || final.Partial || final.FinishReason != genai.FinishReasonStop || final.ErrorCode != "" ||
				len(final.Content.Parts) != 1 || final.Content.Parts[0].Text != "Hello world" || final.Content.Role != "model" {
				t.Fatalf("final %+v; want the whole text, complete", final)
			}
			if u := final.UsageMetadata; u == nil || u.PromptTokenCount != 10 || u.CandidatesTokenCount != 2 || u.TotalTokenCount != 12 {
				t.Errorf("usage %+v; want 10 in, 2 out", u)
			}
			body := api.request(0)
			if body["model"] != "claude-haiku-4-5" || body["max_tokens"] != float64(DefaultMaxTokens) || body["stream"] != true {
				t.Errorf("request model %v, max_tokens %v, stream %v", body["model"], body["max_tokens"], body["stream"])
			}
			if got, _ := json.Marshal(body["system"]); string(got) != `[{"text":"Be brief.","type":"text"}]` {
				t.Errorf("system %s", got)
			}
			if got, _ := json.Marshal(body["messages"]); string(got) != `[{"content":[{"text":"hello","type":"text"}],"role":"user"}]` {
				t.Errorf("messages %s", got)
			}
			for _, absent := range []string{"temperature", "top_p", "top_k", "thinking"} {
				if _, ok := body[absent]; ok {
					t.Errorf("the request sets %s", absent)
				}
			}
			h := api.headers[0]
			if h.Get("Authorization") != "Bearer "+testKey || h.Get("Anthropic-Version") == "" {
				t.Errorf("headers authorization %q, anthropic-version %q; want the key and a version", h.Get("Authorization"), h.Get("Anthropic-Version"))
			}
		})
	}
}

// ask_user comes back as a function call with the tool's ID, name and arguments, its input put together from its JSON pieces,
// after the text the model wrote before it; the request declares the tool with its JSON Schema.
func TestAskUserToolUse(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sse(w, message("tool_use", textBlock(0, "I need one detail."), toolBlock(1, "toolu_01", "ask_user", `{"quest`, `ion": "Which `, `service?"}`))...)
	})
	req := userText("investigate")
	req.Config = &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
		Name: "ask_user", Description: "Ask the user one question and wait for their answer.",
		ParametersJsonSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"question": {Type: "string"}}, Required: []string{"question"}},
	}}}}}
	partials, final, err := collect(t.Context(), api.model(), req, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(partials, "") != "I need one detail." {
		t.Errorf("partials %q; only text streams", partials)
	}
	parts := final.Content.Parts
	if len(parts) != 2 || parts[0].Text != "I need one detail." || parts[1].FunctionCall == nil {
		t.Fatalf("parts %+v; want the text, then the call", parts)
	}
	if call := parts[1].FunctionCall; call.ID != "toolu_01" || call.Name != "ask_user" || call.Args["question"] != "Which service?" {
		t.Errorf("call %+v", call)
	}
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("finish %s; a tool call is a stop", final.FinishReason)
	}
	got, _ := json.Marshal(api.request(0)["tools"])
	if want := `[{"description":"Ask the user one question and wait for their answer.","input_schema":{"properties":{"question":{"type":"string"}},"required":["question"],"type":"object"},"name":"ask_user"}]`; string(got) != want {
		t.Errorf("tools %s; want %s", got, want)
	}
}

// The conversation after a question: the call goes back as the assistant's tool_use, and the answer as the user's tool_result with
// the response as JSON text, ahead of any text in the same message (the API takes tool results first). Messages of one role next to
// each other are one message.
func TestHistoryCarriesTheCallAndItsAnswer(t *testing.T) {
	req := &model.LLMRequest{Contents: []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "investigate"}}},
		{Role: "user", Parts: []*genai.Part{{Text: "the alert"}}},
		{Role: "model", Parts: []*genai.Part{{Text: "One question."}, {FunctionCall: &genai.FunctionCall{ID: "toolu_01", Name: "ask_user", Args: map[string]any{"question": "Which?"}}}}},
		{Role: "user", Parts: []*genai.Part{{Text: "also this"}, {FunctionResponse: &genai.FunctionResponse{ID: "toolu_01", Name: "ask_user", Response: map[string]any{"answer": "checkout"}}}}},
		nil,
		{Role: "model", Parts: []*genai.Part{}},
	}}
	p, err := (&Model{ID: "claude-haiku-4-5", Client: NewClient()}).params(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(p.Messages)
	want := `[{"content":[{"text":"investigate","type":"text"},{"text":"the alert","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"One question.","type":"text"},{"id":"toolu_01","input":{"question":"Which?"},"name":"ask_user","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"tool_use_id":"toolu_01","content":[{"text":"{\"answer\":\"checkout\"}","type":"text"}],"type":"tool_result"},{"text":"also this","type":"text"}],"role":"user"}]`
	if string(got) != want {
		t.Errorf("messages\n%s\nwant\n%s", got, want)
	}
}

// A call with no arguments goes back with an empty object, which the API requires.
func TestCallWithoutArgumentsSendsAnEmptyInput(t *testing.T) {
	p, err := (&Model{ID: "m", Client: NewClient()}).params(&model.LLMRequest{Contents: []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "go"}}},
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "toolu_02", Name: "ping"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := content(t, p.Messages[1]); !strings.Contains(got, `"input":{}`) {
		t.Errorf("assistant message %s; want input {}", got)
	}
}

// Thinking blocks, which a model that thinks writes before its text or its call, ride on the part that follows them (the last part
// when nothing follows) and go back to the API unchanged and in their place, as the API requires of a turn that called a tool.
// They never stream as text.
func TestThinkingBlocksRoundTrip(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sse(w, message("tool_use", thinkingBlock(0, "sig-1"), redactedBlock(1, "opaque-1"), textBlock(2, "Asking."), thinkingBlock(3, "sig-2"),
			toolBlock(4, "toolu_01", "ask_user", `{"question":"Which?"}`), thinkingBlock(5, "sig-3"))...)
	})
	partials, final, err := collect(t.Context(), api.model(), userText("investigate"), true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(partials, "") != "Asking." {
		t.Errorf("partials %q; thinking never streams", partials)
	}
	parts := final.Content.Parts
	if len(parts) != 2 || parts[0].Text != "Asking." || parts[0].Thought || parts[1].FunctionCall == nil || len(parts[0].ThoughtSignature) == 0 ||
		len(parts[1].ThoughtSignature) == 0 {
		t.Fatalf("parts %+v; want the text and the call, each with the thinking before it", parts)
	}
	p, err := api.model().params(&model.LLMRequest{Contents: []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "investigate"}}},
		final.Content,
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "toolu_01", Name: "ask_user", Response: map[string]any{"answer": "checkout"}}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := content(t, p.Messages[1])
	want := `[{"signature":"sig-1","thinking":"weighing it","type":"thinking"},{"data":"opaque-1","type":"redacted_thinking"},{"text":"Asking.","type":"text"},` +
		`{"signature":"sig-2","thinking":"weighing it","type":"thinking"},{"id":"toolu_01","input":{"question":"Which?"},"name":"ask_user","type":"tool_use"},` +
		`{"signature":"sig-3","thinking":"weighing it","type":"thinking"}]`
	if got != want {
		t.Errorf("assistant content\n%s\nwant\n%s", got, want)
	}
}

// A signature the add-on did not write (another provider's, or none of ours) is not sent, and costs the part nothing.
func TestForeignSignatureIsIgnored(t *testing.T) {
	p, err := (&Model{ID: "m", Client: NewClient()}).params(&model.LLMRequest{Contents: []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "go"}}},
		{Role: "model", Parts: []*genai.Part{{Text: "done", ThoughtSignature: []byte("gemini-signature")}, {Thought: true, Text: "reasoning"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := content(t, p.Messages[1]); got != `[{"text":"done","type":"text"}]` {
		t.Errorf("assistant content %s; want the text alone", got)
	}
}

// How the model stopped: a call or the end of its turn is a stop; the token limit and the context window keep what was written as
// MAX_TOKENS; a refusal with nothing written is an error code, which fails the turn; a reply cut off in the middle of a call is an
// error, since the SDK empties the cut input, which would read as a call with no arguments.
func TestStopReasons(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reason  string
		finish  genai.FinishReason
		text    string
		code    string
		wantErr string
	}{
		{"end_turn", "end_turn", genai.FinishReasonStop, "done", "", ""},
		{"stop_sequence", "stop_sequence", genai.FinishReasonStop, "done", "", ""},
		{"max_tokens with text", "max_tokens", genai.FinishReasonMaxTokens, "done", "", ""},
		{"context window", "model_context_window_exceeded", genai.FinishReasonMaxTokens, "done", "", ""},
		{"refusal with text", "refusal", genai.FinishReasonSafety, "done", "", ""},
		{"refusal with nothing", "refusal", genai.FinishReasonSafety, "", "SAFETY", ""},
		{"pause_turn with nothing", "pause_turn", genai.FinishReasonOther, "", "OTHER", ""},
		{"max_tokens in a call", "max_tokens", "", "", "", "cut off in a tool call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var blocks [][]string
			switch {
			case tc.wantErr != "":
				blocks = append(blocks, toolBlock(0, "toolu_01", "ask_user", `{"question":"Whi`))
			case tc.text != "":
				blocks = append(blocks, textBlock(0, tc.text))
			}
			api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) { sse(w, message(tc.reason, blocks...)...) })
			_, final, err := collect(t.Context(), api.model(), userText("go"), true)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v; want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if final.FinishReason != tc.finish || final.ErrorCode != tc.code || !final.TurnComplete {
				t.Errorf("finish %q, error code %q; want %q, %q", final.FinishReason, final.ErrorCode, tc.finish, tc.code)
			}
			var text string
			if final.Content != nil && len(final.Content.Parts) > 0 {
				text = final.Content.Parts[0].Text
			}
			if text != tc.text {
				t.Errorf("text %q; want %q", text, tc.text)
			}
			if tc.code != "" && !strings.Contains(final.ErrorMessage, tc.reason) {
				t.Errorf("error message %q; want one naming the stop reason", final.ErrorMessage)
			}
		})
	}
}

// An error the API answers is the call's status and the error's type, never the error's message, which may quote the request; the
// SDK retries a status it may (here 529, overloaded) MaxRetries times; an error event in the middle of a stream is its type.
func TestAPIErrors(t *testing.T) {
	const secret = "supersecret " + testKey
	for _, tc := range []struct {
		name     string
		script   func(w http.ResponseWriter, n int)
		want     string
		requests int
	}{
		{"bad request", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, secret)
		}, "anthropic request failed: 400 invalid_request_error", 1},
		{"unauthorized", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":%q}}`, secret)
		}, "anthropic request failed: 401 authentication_error", 1},
		{"overloaded, retried", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(529)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"overloaded_error","message":%q}}`, secret)
		}, "anthropic request failed: 529 overloaded_error", MaxRetries + 1},
		{"an error in the stream", func(w http.ResponseWriter, _ int) {
			sse(w, start(), fmt.Sprintf(`error {"type":"error","error":{"type":"overloaded_error","message":%q}}`, secret))
		}, "anthropic stream failed: overloaded_error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, n int) { tc.script(w, n) })
			_, _, err := collect(t.Context(), api.model(), userText("go"), true)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error %v; want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "supersecret") || strings.Contains(err.Error(), testKey) {
				t.Fatalf("the error %q quotes the API's message", err)
			}
			if api.count() != tc.requests {
				t.Errorf("requests %d; want %d", api.count(), tc.requests)
			}
		})
	}
}

// A stream that ends before message_stop is an error, not a complete message.
func TestStreamCutShortIsAnError(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sse(w, append([]string{start()}, textBlock(0, "half")...)...)
	})
	if _, _, err := collect(t.Context(), api.model(), userText("go"), true); err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("error %v; want a stream that ended before the message stopped", err)
	}
}

// Cancelling the turn's context stops the stream, and the turn ends with the context's error.
func TestCancellationEndsTheStream(t *testing.T) {
	var gone atomic.Bool
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		sse(w, append([]string{start()}, textBlock(0, "first")[:2]...)...)
		<-r.Context().Done()
		gone.Store(true)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var err error
	var partials int
	for out, e := range api.model().GenerateContent(ctx, userText("go"), true) {
		if e != nil {
			err = e
			break
		}
		if out.Partial {
			partials++
			cancel()
		}
	}
	if partials != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("partials %d, error %v; want one partial, then the context's cancellation", partials, err)
	}
	api.srv.Close() // waits for the handler
	if !gone.Load() {
		t.Error("the request was not cancelled")
	}
}

// What the add-on cannot send is refused before a request: sampling settings, which the API version the SDK sends refuses on every
// model, and a part that is neither text nor a function call or response.
func TestRefusedRequests(t *testing.T) {
	temp := float32(0.2)
	for _, tc := range []struct {
		name string
		req  *model.LLMRequest
		want string
	}{
		{"temperature", &model.LLMRequest{Contents: userText("go").Contents, Config: &genai.GenerateContentConfig{Temperature: &temp}}, "temperature"},
		{"top_p", &model.LLMRequest{Contents: userText("go").Contents, Config: &genai.GenerateContentConfig{TopP: &temp}}, "top_p"},
		{"an image", &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1}}}}}}}, "text and function tools only"},
		{"no request", nil, "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, func(http.ResponseWriter, *http.Request, int) {})
			_, _, err := collect(t.Context(), api.model(), tc.req, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v; want one containing %q", err, tc.want)
			}
			if api.count() != 0 {
				t.Errorf("%d requests were sent", api.count())
			}
		})
	}
	if _, err := (&Model{Client: NewClient()}).params(userText("go")); err == nil {
		t.Error("a model with no ID made a request")
	}
}

// The request's own token limit wins over the default, and its stop sequences are sent.
func TestRequestLimitsAndStopSequences(t *testing.T) {
	p, err := (&Model{ID: "m", Client: NewClient()}).params(&model.LLMRequest{Contents: userText("go").Contents,
		Config: &genai.GenerateContentConfig{MaxOutputTokens: 512, StopSequences: []string{"END"}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxTokens != 512 || len(p.StopSequences) != 1 || p.StopSequences[0] != "END" {
		t.Errorf("max_tokens %d, stop sequences %v", p.MaxTokens, p.StopSequences)
	}
}

// The client reads the key and the base URL from the SDK's environment variables, as the chart and ANTHROPIC_BASE_URL set them.
func TestClientReadsTheSDKsEnvironment(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		sse(w, message("end_turn", textBlock(0, "ok"))...)
	})
	t.Setenv("ANTHROPIC_API_KEY", testKey)
	t.Setenv("ANTHROPIC_BASE_URL", api.srv.URL)
	if _, _, err := collect(t.Context(), &Model{ID: "claude-haiku-4-5", Client: NewClient()}, userText("go"), true); err != nil {
		t.Fatal(err)
	}
	if got := api.headers[0].Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("authorization %q; want the environment's key", got)
	}
}

func TestName(t *testing.T) {
	if got := (&Model{ID: "claude-haiku-4-5"}).Name(); got != "claude-haiku-4-5" {
		t.Errorf("Name %q", got)
	}
}

func TestTokensHoldsAtGenaisWidth(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want int32
	}{{0, 0}, {42, 42}, {-1, 0}, {math.MaxInt32, math.MaxInt32}, {math.MaxInt32 + 1, math.MaxInt32}} {
		if got := tokens(c.in); got != c.want {
			t.Errorf("tokens(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
