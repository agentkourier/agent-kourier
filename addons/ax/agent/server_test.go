package agent

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type fakeModel struct {
	calls   atomic.Int32
	hold    chan struct{}
	entered chan struct{}
}

func (m *fakeModel) Name() string { return "mock-haiku" }
func (m *fakeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls.Add(1)
		if m.hold != nil {
			select {
			case m.entered <- struct{}{}:
			default:
			}
			select {
			case <-m.hold:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		last := req.Contents[len(req.Contents)-1]
		for _, p := range last.Parts {
			if p.FunctionResponse != nil {
				yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Answer received: " + p.FunctionResponse.Response["answer"].(string)}}}, TurnComplete: true}, nil)
				return
			}
		}
		for _, p := range last.Parts {
			if strings.Contains(p.Text, "question") {
				yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "ask_user", Args: map[string]any{"question": "Which service?"}}}}}, TurnComplete: true}, nil)
				return
			}
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Hello "}}}, Partial: true}, nil)
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Hello world"}}}, TurnComplete: true}, nil)
	}
}

type authTransport struct {
	base http.RoundTripper
	user string
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.user)
	return t.base.RoundTrip(cloned)
}
func start(t *testing.T, dir string, m *fakeModel) (*Server, *httptest.Server, *a2aclient.Client) {
	t.Helper()
	s, err := New(t.Context(), Config{StateDir: dir, Model: m, Authenticate: func(_ context.Context, header string) (string, error) { return VerifyBearer(header) }})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler)
	client := connect(t, httpServer, "binding")
	t.Cleanup(func() { httpServer.Close(); _ = s.Close() })
	return s, httpServer, client
}
func connect(t *testing.T, s *httptest.Server, user string) *a2aclient.Client {
	t.Helper()
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true}, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(s.URL+"/a2a", a2a.TransportProtocolJSONRPC)}}
	client, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(&http.Client{Transport: authTransport{s.Client().Transport, user}}))
	if err != nil {
		t.Fatal(err)
	}
	return client
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
	if id == "" {
		t.Fatal("missing task ID")
	}
	task, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func message(id, text string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ID = id
	return m
}
func taskText(task *a2a.Task) string {
	var b strings.Builder
	for _, art := range task.Artifacts {
		for _, part := range art.Parts {
			b.WriteString(part.Text())
		}
	}
	return b.String()
}

func TestChatReplayOwnerIsolationAndReopen(t *testing.T) {
	dir := t.TempDir()
	m := &fakeModel{}
	s, httpServer, c := start(t, dir, m)
	task := send(t, c, message("m1", "hello"))
	if task.Status.State != a2a.TaskStateCompleted || taskText(task) != "Hello world" {
		t.Fatalf("task: %+v", task)
	}
	again := message("m1", "hello")
	again.ContextID = task.ContextID
	replay := send(t, c, again)
	if replay.ID != task.ID || m.calls.Load() != 1 {
		t.Fatal("replay executed again")
	}
	page, err := c.ListTasks(t.Context(), &a2a.ListTasksRequest{ContextID: task.ContextID})
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].History[0].ID != "m1" {
		t.Fatalf("recovery lookup: %+v %v", page, err)
	}
	other := connect(t, httpServer, "other-binding")
	if _, err := other.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID}); err == nil {
		t.Fatal("another owner read task")
	}
	httpServer.Close()
	_ = s.Close()
	_, _, c2 := start(t, dir, m)
	replay = send(t, c2, again)
	if replay.ID != task.ID || m.calls.Load() != 1 {
		t.Fatal("reopen lost message dedup")
	}
	next := message("m2", "follow up")
	next.ContextID = task.ContextID
	if nextTask := send(t, c2, next); nextTask.ContextID != task.ContextID {
		t.Fatal("context lost on reopen")
	}
}

func TestTextQuestionSurvivesReopenAndAnswersSameTask(t *testing.T) {
	dir := t.TempDir()
	m := &fakeModel{}
	s, httpServer, c := start(t, dir, m)
	task := send(t, c, message("q1", "ask a question"))
	if task.Status.State != a2a.TaskStateInputRequired || task.Status.Message.Parts[0].Text() != "Which service?" {
		t.Fatalf("missing text question: %+v", task)
	}
	httpServer.Close()
	_ = s.Close()
	_, _, c2 := start(t, dir, m)
	answer := message("answer1", "checkout")
	answer.TaskID = task.ID
	answer.ContextID = task.ContextID
	completed := send(t, c2, answer)
	if completed.ID != task.ID || completed.Status.State != a2a.TaskStateCompleted || !strings.Contains(taskText(completed), "checkout") {
		t.Fatalf("answer failed: %+v", completed)
	}
	before := m.calls.Load()
	send(t, c2, answer)
	if m.calls.Load() != before {
		t.Fatal("answer replay executed twice")
	}
}

func TestStoreSettlesInterruptedWorkAndRejectsStaleVersion(t *testing.T) {
	s, _, _ := start(t, t.TempDir(), &fakeModel{})
	ctx, cc := a2asrv.NewCallContext(t.Context(), nil)
	cc.User = a2asrv.NewAuthenticatedUser("binding", nil)
	task := &a2a.Task{ID: "interrupted", ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}, History: []*a2a.Message{message("accepted", "hello")}}
	version, err := s.Store.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: version + 1}); err == nil {
		t.Fatal("stale update accepted")
	}
	if err := s.Store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Store.Get(ctx, task.ID)
	if err != nil || stored.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("interrupted task not settled: %+v %v", stored, err)
	}
}

func TestCancelActiveTaskAndRejectUnauthenticatedCard(t *testing.T) {
	m := &fakeModel{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	_, httpServer, c := start(t, t.TempDir(), m)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+"/a2a/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthenticated card accessible")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ids := make(chan a2a.TaskID, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for event := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("cancel-me", "hello")}) {
			if task, ok := event.(*a2a.Task); ok {
				select {
				case ids <- task.ID:
				default:
				}
			}
		}
	}()
	var id a2a.TaskID
	select {
	case id = <-ids:
	case <-ctx.Done():
		t.Fatal("no initial task")
	}
	select {
	case <-m.entered:
	case <-ctx.Done():
		t.Fatal("model did not start")
	}
	task, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
	if err != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("cancel: %+v, %v", task, err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("canceled stream did not end")
	}
}
