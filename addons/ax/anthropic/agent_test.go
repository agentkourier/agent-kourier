package anthropic_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/anthropic"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// fixedCaller accepts every request as one caller.
type fixedCaller struct{}

func (fixedCaller) Verify(*http.Request) (string, error) { return "binding", nil }

// serveAgent serves the agent on model with the SQLite store in dir, as the command does, until the test ends or stop is called.
func serveAgent(t *testing.T, dir string, model *anthropic.Model) (client *a2aclient.Client, stop func()) {
	t.Helper()
	st, err := store.OpenSQLite(t.Context(), filepath.Join(dir, "ax.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SettleInterrupted(t.Context(), runtime.InterruptedMessage("agent")); err != nil {
		t.Fatal(err)
	}
	exec, err := agent.NewTextExecutor(model, st.Sessions())
	if err != nil {
		t.Fatal(err)
	}
	check := &auth.Check{Verifier: fixedCaller{}, Bearer: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(agent.Addon("test", nil).Handler(exec, st, check, "http://ax.agents.svc:8090/a2a", nil))
	var once sync.Once
	stop = func() { once.Do(func() { srv.Close(); _ = st.Close() }) }
	t.Cleanup(stop)
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL+"/a2a", a2a.TransportProtocolJSONRPC)}}
	client, err = a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return client, stop
}

func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	var id a2a.TaskID
	for e, err := range c.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
		if err != nil {
			t.Fatal(err)
		}
		switch v := e.(type) {
		case *a2a.Task:
			id = v.ID
		case *a2a.TaskStatusUpdateEvent:
			id = v.TaskID
		case *a2a.TaskArtifactUpdateEvent:
			id = v.TaskID
		}
	}
	task, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// The whole question through the agent on the Anthropic model, with a model that thinks: the turn pauses on ask_user with its
// question as text; after a restart the answer resumes the same task; and the second request carries the first turn as the API
// requires, its thinking block unchanged before the call, then the answer as the call's result.
func TestAskUserAcrossARestartOnTheMessagesAPI(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_PROFILE", "ANTHROPIC_CUSTOM_HEADERS"} {
		t.Setenv(name, "")
	}
	var mu sync.Mutex
	var bodies []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{`message_start {"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`}
		if n == 0 {
			events = append(events,
				`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
				`content_block_stop {"type":"content_block_stop","index":0}`,
				`content_block_start {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"ask_user","input":{}}}`,
				`content_block_delta {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"question\":\"Which service?\"}"}}`,
				`content_block_stop {"type":"content_block_stop","index":1}`,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`)
		} else {
			events = append(events,
				`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Looking at checkout."}}`,
				`content_block_stop {"type":"content_block_stop","index":0}`,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		}
		events = append(events, `message_stop {"type":"message_stop"}`)
		for _, e := range events {
			name, data, _ := strings.Cut(e, " ")
			_, _ = io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n")
		}
	}))
	defer api.Close()
	model := &anthropic.Model{ID: "claude-haiku-4-5", Client: anthropic.NewClient(option.WithBaseURL(api.URL), option.WithAPIKey("test-key-not-real"))}
	dir := t.TempDir()

	c, stop := serveAgent(t, dir, model)
	question := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("checkout is slow")))
	if question.Status.State != a2a.TaskStateInputRequired || question.Status.Message.Parts[0].Text() != "Which service?" {
		t.Fatalf("task %+v; want input-required with the question as text", question.Status)
	}
	stop()

	c, _ = serveAgent(t, dir, model)
	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("checkout"))
	answer.TaskID, answer.ContextID = question.ID, question.ContextID
	done := send(t, c, answer)
	if done.ID != question.ID || done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task %s %s; want the question's task, completed", done.ID, done.Status.State)
	}
	var text strings.Builder
	for _, art := range done.Artifacts {
		for _, p := range art.Parts {
			text.WriteString(p.Text())
		}
	}
	if !strings.Contains(text.String(), "Looking at checkout.") {
		t.Errorf("artifacts %q; want the reply", text.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests; want the question's and the answer's", len(bodies))
	}
	got, _ := json.Marshal(bodies[1]["messages"])
	want := `[{"content":[{"text":"checkout is slow","type":"text"}],"role":"user"},` +
		`{"content":[{"signature":"sig-1","thinking":"","type":"thinking"},{"id":"toolu_01","input":{"question":"Which service?"},"name":"ask_user","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":[{"text":"{\"answer\":\"checkout\"}","type":"text"}],"tool_use_id":"toolu_01","type":"tool_result"}],"role":"user"}]`
	if string(got) != want {
		t.Errorf("the answer's request\n%s\nwant\n%s", got, want)
	}
}
