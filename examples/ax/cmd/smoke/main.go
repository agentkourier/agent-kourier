// Command smoke exercises the reference A2A server with a real Bedrock model on loopback.
package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/agentkourier/agent-kourier/examples/ax/agent"
	"github.com/agentkourier/agent-kourier/examples/ax/bedrock"
	"google.golang.org/adk/v2/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Bedrock A2A smoke failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Getenv("MODEL_REGION") == "" || os.Getenv("MODEL_NAME") == "" {
		return errors.New("MODEL_REGION and MODEL_NAME are required")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(os.Getenv("MODEL_REGION")))
	if err != nil {
		return errors.New("AWS configuration failed")
	}
	dir, err := os.MkdirTemp("", "ax-smoke-")
	if err != nil {
		return errors.New("state directory unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	s, err := agent.New(ctx, agent.Config{StateDir: dir, Model: observedModel{&bedrock.Model{ID: os.Getenv("MODEL_NAME"), Client: bedrockruntime.NewFromConfig(cfg)}}, Authenticate: func(_ context.Context, header string) (string, error) {
		if header != "Bearer local-smoke" {
			return "", a2a.ErrUnauthenticated
		}
		return "smoke", nil
	}})
	if err != nil {
		return errors.New("agent startup failed")
	}
	defer func() { _ = s.Close() }()
	h := httptest.NewServer(s.Handler)
	defer h.Close()
	card := &a2a.AgentCard{Name: "smoke", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true}, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(h.URL+"/a2a", a2a.TransportProtocolJSONRPC)}}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(&http.Client{Transport: transport{h.Client().Transport}}))
	if err != nil {
		return errors.New("A2A client startup failed")
	}
	send := func(msg *a2a.Message) (*a2a.Task, error) {
		var id a2a.TaskID
		for event, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				return nil, errors.New("A2A model turn failed")
			}
			switch e := event.(type) {
			case *a2a.Task:
				id = e.ID
			case *a2a.TaskStatusUpdateEvent:
				id = e.TaskID
			case *a2a.TaskArtifactUpdateEvent:
				id = e.TaskID
			}
		}
		return c.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	}
	task, err := send(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Use ask_user to ask me which service needs investigation. Wait for my answer.")))
	if err != nil || task.Status.State != a2a.TaskStateInputRequired {
		return errors.New("expected a plain text question")
	}
	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("checkout. No investigation is needed. Acknowledge my answer and finish."))
	answer.TaskID, answer.ContextID = task.ID, task.ContextID
	done, err := send(answer)
	if err != nil || done.Status.State != a2a.TaskStateCompleted {
		return errors.New("answer did not complete the same task")
	}
	if done.ID != task.ID {
		return errors.New("task identity changed")
	}
	replay, err := send(answer)
	if err != nil || replay.ID != task.ID || replay.Status.State != a2a.TaskStateCompleted {
		return errors.New("replay failed")
	}
	fmt.Println("PASS: Bedrock streamed a text question, accepted its answer on the same A2A task, and replayed the completed task.")
	return nil
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer local-smoke")
	return t.base.RoundTrip(r)
}

// Only the adapter's sanitized errors are printed, never AWS response messages.
type observedModel struct{ *bedrock.Model }

func (m observedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for response, err := range m.Model.GenerateContent(ctx, req, stream) {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if !yield(response, err) {
				return
			}
		}
	}
}
