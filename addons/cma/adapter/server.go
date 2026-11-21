package adapter

import (
	"context"
	"iter"
	"log/slog"
	"net/http"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
)

// NewRequestHandler is the A2A request handler over exec, with exec.Store as its task store; opts add to it (a call
// interceptor, say). A message whose ID the store has recorded joins that message's task instead of starting another
// turn, after a restart too: Agent Kourier resends a message with the same ID when it cannot tell whether the first
// send arrived.
func NewRequestHandler(exec *Executor, opts ...a2asrv.RequestHandlerOption) a2asrv.RequestHandler {
	inner := a2asrv.NewHandler(exec, append([]a2asrv.RequestHandlerOption{a2asrv.WithTaskStore(exec.Store)}, opts...)...)
	return &replayHandler{RequestHandler: inner, messages: exec.Store, log: exec.logger(), tel: exec.Telemetry}
}

// Card is the adapter's agent card. url is the JSON-RPC endpoint clients reach it at; version is the adapter's.
func Card(name, description, url, version string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:                name,
		Description:         description,
		Version:             version,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities: a2a.AgentCapabilities{
			Streaming:  true,
			Extensions: []a2a.AgentExtension{{URI: HITLExtension, Description: "Tool approvals as structured pauses"}},
		},
	}
}

// CardPath is where Mux serves the card, under the JSON-RPC endpoint's path.
const CardPath = "/.well-known/agent-card.json"

// Mux serves the card at path + CardPath and rpc, the JSON-RPC handler behind any caller check, at path, as Agent
// Kourier's a2a dialect expects of an exact endpoint.
func Mux(path string, card *a2a.AgentCard, rpc http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(path+CardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(path, rpc)
	return mux
}

// messageStore records which task each message ID started or joined.
type messageStore interface {
	MessageTask(ctx context.Context, messageID string) (a2a.TaskID, bool, error)
	RecordMessage(ctx context.Context, messageID string, taskID a2a.TaskID) error
}

type replayHandler struct {
	a2asrv.RequestHandler
	messages messageStore
	log      *slog.Logger
	tel      *telemetry.Telemetry
	// mu is held from a message's lookup until its task is recorded, so that a resend that races the first send in
	// this process finds the task. The store is for a resend after a restart.
	mu sync.Mutex
}

func (h *replayHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if req == nil || req.Message == nil || req.Message.ID == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		h.mu.Lock()
		id, ok, err := h.messages.MessageTask(ctx, req.Message.ID)
		if err != nil {
			h.mu.Unlock()
			yield(nil, err)
			return
		}
		if ok {
			h.mu.Unlock()
			h.tel.MessageReplay()
			h.rejoin(ctx, id, yield)
			return
		}
		// Refuse an answer that does not decide the paused task's tool calls here, before the executor runs: an
		// executor error fails the task, and the session would wait on the calls for good.
		if req.Message.TaskID != "" {
			if err := h.checkAnswer(ctx, req.Message); err != nil {
				h.mu.Unlock()
				yield(nil, err)
				return
			}
		}
		// Hold the lock until the turn has a task, so a resend that races the first send finds it.
		locked := true
		defer func() {
			if locked {
				h.mu.Unlock()
			}
		}()
		for ev, err := range h.RequestHandler.SendStreamingMessage(ctx, req) {
			if locked && err == nil && ev != nil {
				if id := ev.TaskInfo().TaskID; id != "" {
					if err := h.messages.RecordMessage(context.WithoutCancel(ctx), req.Message.ID, id); err != nil {
						h.log.Warn("record a message ID; a resend of the message will start another turn", "message", req.Message.ID, "task", id, "error", err)
					}
					h.mu.Unlock()
					locked = false
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// checkAnswer reports why msg cannot answer the task it names, when that task waits on tool approvals.
func (h *replayHandler) checkAnswer(ctx context.Context, msg *a2a.Message) error {
	task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: msg.TaskID})
	if err != nil || task.Status.State != a2a.TaskStateInputRequired {
		return nil // the inner handler answers for a task it does not have or one that is not paused
	}
	_, _, err = (&Executor{}).outbound(&a2asrv.ExecutorContext{Message: msg, StoredTask: task})
	return err
}

// rejoin answers a resent message with its task: the task as it stands if it has settled or paused, and otherwise
// the rest of its stream.
func (h *replayHandler) rejoin(ctx context.Context, id a2a.TaskID, yield func(a2a.Event, error) bool) {
	task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		yield(nil, err)
		return
	}
	if task.Status.State.Terminal() || task.Status.State == a2a.TaskStateInputRequired {
		yield(task, nil)
		return
	}
	for ev, err := range h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: id}) {
		if !yield(ev, err) {
			return
		}
	}
}

func (h *replayHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	var id a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, req) {
		if err != nil {
			return nil, err
		}
		id = ev.TaskInfo().TaskID
	}
	return h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
}
