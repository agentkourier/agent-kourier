package adapter

import (
	"context"
	"iter"
	"net/http"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// NewRequestHandler is the A2A request handler over exec, with an in-memory task store. A message whose ID the
// handler has seen joins that message's task instead of starting another turn: Agent Kourier resends a message with
// the same ID when it cannot tell whether the first send arrived.
func NewRequestHandler(exec *Executor) a2asrv.RequestHandler {
	inner := a2asrv.NewHandler(exec, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	return &replayHandler{RequestHandler: inner, byMessage: map[string]a2a.TaskID{}}
}

// Card is the adapter's agent card. url is the JSON-RPC endpoint clients reach it at.
func Card(name, description, url string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:                name,
		Description:         description,
		Version:             "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities: a2a.AgentCapabilities{
			Streaming:  true,
			Extensions: []a2a.AgentExtension{{URI: HITLExtension, Description: "Tool approvals as structured pauses"}},
		},
	}
}

// Mux serves the card at path + "/.well-known/agent-card.json" and JSON-RPC at path, as Agent Kourier's a2a dialect
// expects of an exact endpoint.
func Mux(path string, card *a2a.AgentCard, handler a2asrv.RequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(path+"/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(path, a2asrv.NewJSONRPCHandler(handler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type replayHandler struct {
	a2asrv.RequestHandler
	mu        sync.Mutex
	byMessage map[string]a2a.TaskID
}

func (h *replayHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if req == nil || req.Message == nil || req.Message.ID == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		h.mu.Lock()
		if id, ok := h.byMessage[req.Message.ID]; ok {
			h.mu.Unlock()
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
					h.byMessage[req.Message.ID] = id
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
