// Package agent is the ADK agent on Bedrock and its A2A request handler. A message whose ID the store has recorded
// joins that message's task instead of starting another turn, and an ask_user question is a plain-text pause that the
// next message on the task answers.
package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/agentkourier/agent-kourier/addons/ax/auth"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
)

// Store is what the agent keeps across a restart: the A2A tasks (by caller), the message IDs that started or joined
// them, and ADK's sessions. store.Store implements it on SQLite and Postgres.
type Store interface {
	taskstore.Store
	// MessageTask returns the call's caller's task that a message ID started or joined, or nil when it is not
	// recorded.
	MessageTask(ctx context.Context, messageID string) (*a2a.Task, error)
	// Sessions is ADK's session service.
	Sessions() session.Service
}

var _ Store = (*store.Store)(nil)

// Config describes the agent.
type Config struct {
	// Model answers the turns.
	Model model.LLM
	// Store keeps the tasks and the sessions.
	Store Store
	// Check is the caller check every request to the A2A endpoint and to the card must pass.
	Check *auth.Check
	// PublicURL is the JSON-RPC endpoint the card names.
	PublicURL string
	// Version is the agent's, which the card names.
	Version string
}

// Server is the agent's A2A surface.
type Server struct {
	// Handler serves the JSON-RPC endpoint at A2APath and the card at A2APath + CardPath, both behind the caller check.
	Handler http.Handler
}

// A2APath is the JSON-RPC endpoint's path; CardPath, the card's under it.
const (
	A2APath  = "/a2a"
	CardPath = "/.well-known/agent-card.json"
)

// New builds the ADK agent, with its sessions in the store, behind an A2A request handler whose task store is the
// store too.
func New(cfg Config) (*Server, error) {
	if cfg.Model == nil || cfg.Store == nil || cfg.Check == nil || cfg.PublicURL == "" {
		return nil, errors.New("model, store, caller check and public URL are required")
	}
	ask, err := functiontool.New(functiontool.Config{Name: "ask_user", Description: "Ask the user one question and wait for their answer.", IsLongRunning: true}, func(_ adkagent.Context, _ struct {
		Question string `json:"question"`
	}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("the ask_user tool: %w", err)
	}
	root, err := llmagent.New(llmagent.Config{Name: "ax_assistant", Description: "AX chat and investigation assistant", Model: cfg.Model, Instruction: "Answer chat messages and investigate the alert information provided. Ask for missing information using ask_user. You have no access to external systems or mutation tools; distinguish supplied evidence from inference.", Tools: []tool.Tool{ask}})
	if err != nil {
		return nil, fmt.Errorf("the agent: %w", err)
	}
	executor := &TextExecutor{Inner: adka2a.NewExecutor(adka2a.ExecutorConfig{RunnerConfig: runner.Config{AppName: store.AppName, Agent: root, SessionService: cfg.Store.Sessions()}, RunConfig: adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}, OutputMode: adka2a.OutputArtifactPerEvent})}
	base := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(cfg.Store))
	handler := &a2asrv.InterceptedHandler{Handler: &replayHandler{RequestHandler: base, store: cfg.Store}, Interceptors: []a2asrv.CallInterceptor{auth.Interceptor{}}}
	card := &a2a.AgentCard{Name: "ax-adk-bedrock", Description: "An ADK agent on Amazon Bedrock, served over A2A", Version: cfg.Version, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(cfg.PublicURL, a2a.TransportProtocolJSONRPC)}, DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"}, Capabilities: a2a.AgentCapabilities{Streaming: true}}
	mux := http.NewServeMux()
	mux.Handle(A2APath+CardPath, cfg.Check.Wrap(a2asrv.NewStaticAgentCardHandler(card)))
	mux.Handle(A2APath, cfg.Check.Wrap(a2asrv.NewJSONRPCHandler(handler)))
	return &Server{Handler: mux}, nil
}

// replayHandler serializes acceptance, not execution, so duplicate message IDs join the original task.
type replayHandler struct {
	a2asrv.RequestHandler
	store Store
	mu    sync.Mutex
}

func (h *replayHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		h.mu.Lock()
		locked := true
		defer func() {
			if locked {
				h.mu.Unlock()
			}
		}()
		if req == nil || req.Message == nil || req.Message.ID == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		old, err := h.store.MessageTask(ctx, req.Message.ID)
		if err != nil {
			yield(nil, err)
			return
		}
		if old != nil {
			h.mu.Unlock()
			locked = false
			if req.Message.ContextID != "" && req.Message.ContextID != old.ContextID {
				yield(nil, a2a.ErrInvalidParams)
				return
			}
			if req.Message.TaskID != "" && req.Message.TaskID != old.ID {
				yield(nil, a2a.ErrInvalidParams)
				return
			}
			if old.Status.State.Terminal() || old.Status.State == a2a.TaskStateInputRequired {
				yield(old, nil)
				return
			}
			for event, err := range h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: old.ID}) {
				if !yield(event, err) {
					return
				}
			}
			return
		}
		for event, err := range h.RequestHandler.SendStreamingMessage(ctx, req) {
			if locked {
				h.mu.Unlock()
				locked = false
			}
			if !yield(event, err) {
				return
			}
		}
	}
}
func (h *replayHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	var result a2a.SendMessageResult
	for event, err := range h.SendStreamingMessage(ctx, req) {
		if err != nil {
			return nil, err
		}
		if task, ok := event.(*a2a.Task); ok {
			result = task
		}
	}
	if result == nil {
		return nil, errors.New("task result missing")
	}
	return h.GetTask(ctx, &a2a.GetTaskRequest{ID: result.(*a2a.Task).ID})
}
