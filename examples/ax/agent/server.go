// Package agent provides the durable ADK A2A reference server for AX.
package agent

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config describes one persistent agent. Authenticate must verify the request's service identity.
type Config struct {
	StateDir     string
	Model        model.LLM
	Authenticate func(context.Context, string) (string, error)
}

// Server owns the HTTP handler and its persistent database.
type Server struct {
	Handler http.Handler
	Store   *TaskStore
	Close   func() error
}

// New builds ADK with persistent sessions and an owner-scoped A2A task store.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Authenticate == nil || cfg.Model == nil || cfg.StateDir == "" {
		return nil, errors.New("model, authentication and state directory are required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(cfg.StateDir, "agent.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	fail := func(err error) (*Server, error) { _ = sqlDB.Close(); return nil, err }
	if _, err := sqlDB.ExecContext(ctx, `PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL`); err != nil {
		return fail(err)
	}
	sessions, err := database.NewSessionServiceFromDB(db)
	if err != nil {
		return fail(err)
	}
	if err := database.AutoMigrate(sessions); err != nil {
		return fail(err)
	}
	tasks := &TaskStore{DB: sqlDB}
	if err := tasks.Init(ctx); err != nil {
		return fail(err)
	}
	ask, err := functiontool.New(functiontool.Config{Name: "ask_user", Description: "Ask the user one question and wait for their answer.", IsLongRunning: true}, func(_ adkagent.Context, _ struct {
		Question string `json:"question"`
	}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		return fail(err)
	}
	root, err := llmagent.New(llmagent.Config{Name: "ax_assistant", Description: "AX chat and investigation assistant", Model: cfg.Model, Instruction: "Answer chat messages and investigate the alert information provided. Ask for missing information using ask_user. You have no access to external systems or mutation tools; distinguish supplied evidence from inference.", Tools: []tool.Tool{ask}})
	if err != nil {
		return fail(err)
	}
	executor := &TextExecutor{Inner: adka2a.NewExecutor(adka2a.ExecutorConfig{RunnerConfig: runner.Config{AppName: "agent-kourier-ax", Agent: root, SessionService: sessions}, RunConfig: adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}, OutputMode: adka2a.OutputArtifactPerEvent})}
	base := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(tasks))
	handler := &a2asrv.InterceptedHandler{Handler: &replayHandler{RequestHandler: base, store: tasks}, Interceptors: []a2asrv.CallInterceptor{&authentication{}}}
	mux := http.NewServeMux()
	card := &a2a.AgentCard{Name: "ax-adk-bedrock", Description: "ADK agent in AX using Bedrock", Version: "1.0.0", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://localhost/a2a", a2a.TransportProtocolJSONRPC)}, DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"}, Capabilities: a2a.AgentCapabilities{Streaming: true}}
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)
	mux.Handle("/a2a/.well-known/agent-card.json", authenticateHTTP(cfg.Authenticate, cardHandler))
	mux.Handle("/a2a", authenticateHTTP(cfg.Authenticate, a2asrv.NewJSONRPCHandler(handler)))
	return &Server{Handler: mux, Store: tasks, Close: sqlDB.Close}, nil
}

type authenticatedSubjectKey struct{}

// authenticateHTTP rejects requests before A2A dispatch, so an unavailable issuer
// cannot be mistaken for bad credentials or an agent's rejected answer.
func authenticateHTTP(verify func(context.Context, string) (string, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := verify(r.Context(), values[0])
		if errors.Is(err, ErrAuthenticationUnavailable) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if err != nil || user == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authenticatedSubjectKey{}, user)))
	})
}

type authentication struct {
	a2asrv.PassthroughCallInterceptor
}

func (a *authentication) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	user, _ := ctx.Value(authenticatedSubjectKey{}).(string)
	if user == "" {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	cc.User = a2asrv.NewAuthenticatedUser(user, nil)
	return ctx, nil, nil
}

// replayHandler serializes acceptance, not execution, so duplicate message IDs join the original task.
type replayHandler struct {
	a2asrv.RequestHandler
	store *TaskStore
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
		old, err := h.store.FindMessage(ctx, req.Message.ID)
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

// VerifyBearer checks the scheme without exposing the credential in an error.
func VerifyBearer(header string) (string, error) {
	prefix, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") || token == "" {
		return "", a2a.ErrUnauthenticated
	}
	return token, nil
}
