// Command smoke exercises the reference A2A server with a real model on loopback: Bedrock's (MODEL_PROVIDER bedrock or
// unset, MODEL_REGION, MODEL_NAME, AWS credentials) or the Anthropic API's (MODEL_PROVIDER anthropic, MODEL_NAME,
// ANTHROPIC_API_KEY).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/anthropic"
	"github.com/agentkourier/agent-kourier/addons/ax/bedrock"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"google.golang.org/adk/v2/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "A2A smoke failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	llm, err := smokeModel(ctx)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ax-smoke-")
	if err != nil {
		return errors.New("state directory unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	st, err := store.OpenSQLite(ctx, filepath.Join(dir, "agent.db"), store.Options{})
	if err != nil {
		return errors.New("store startup failed")
	}
	defer func() { _ = st.Close() }()
	check := &auth.Check{Verifier: smokeCaller{}, Bearer: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s, err := agent.New(agent.Config{Model: printingModel{llm},
		Store: st, Check: check, PublicURL: "http://127.0.0.1/a2a", Version: "smoke"})
	if err != nil {
		return errors.New("agent startup failed")
	}
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
	fmt.Println("PASS: the model streamed a text question, accepted its answer on the same A2A task, and replayed the completed task.")
	return nil
}

// smokeModel is the model the environment names.
func smokeModel(ctx context.Context) (model.LLM, error) {
	name := os.Getenv("MODEL_NAME")
	if name == "" {
		return nil, errors.New("MODEL_NAME is required")
	}
	switch os.Getenv("MODEL_PROVIDER") {
	case "anthropic":
		return &anthropic.Model{ID: name, Client: anthropic.NewClient()}, nil
	case "", "bedrock":
		if os.Getenv("MODEL_REGION") == "" {
			return nil, errors.New("MODEL_REGION is required with Bedrock")
		}
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(os.Getenv("MODEL_REGION")))
		if err != nil {
			return nil, errors.New("AWS configuration failed")
		}
		return &bedrock.Model{ID: name, Client: bedrockruntime.NewFromConfig(cfg)}, nil
	}
	return nil, errors.New("MODEL_PROVIDER is not bedrock or anthropic")
}

// smokeCaller accepts the smoke's own fixed bearer token.
type smokeCaller struct{}

func (smokeCaller) Verify(r *http.Request) (string, error) {
	if r.Header.Get("Authorization") != "Bearer local-smoke" {
		return "", auth.ErrInvalid
	}
	return "smoke", nil
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer local-smoke")
	return t.base.RoundTrip(r)
}

// Only the adapter's sanitized errors are printed, never the provider's response messages.
type printingModel struct{ model.LLM }

func (m printingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for response, err := range m.LLM.GenerateContent(ctx, req, stream) {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if !yield(response, err) {
				return
			}
		}
	}
}
