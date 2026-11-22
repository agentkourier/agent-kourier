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
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
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

// Mux serves the card at path + CardPath and rpc, the JSON-RPC handler, at path, as Agent Kourier's a2a dialect
// expects of an exact endpoint. Both are behind check, which Agent Kourier passes at the card too: it sends the
// Binding's credentials on every request to the endpoint, the card's fetch included.
func Mux(path string, card *a2a.AgentCard, rpc http.Handler, check *auth.Check) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(path+CardPath, check.Wrap(a2asrv.NewStaticAgentCardHandler(card)))
	mux.Handle(path, check.Wrap(rpc))
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
	// sending holds, by message ID, a send from its lookup until its task is recorded, so that a resend that races the
	// first send in this process waits for the task and finds it. Sends of other message IDs never wait on it. The
	// store is for a resend after a restart.
	mu      sync.Mutex
	sending map[string]chan struct{}
}

// claim waits until no other send of the message ID is between its lookup and its task's record, then makes this
// send that one. The returned func ends the claim; it may be called more than once.
func (h *replayHandler) claim(ctx context.Context, messageID string) (func(), error) {
	for {
		h.mu.Lock()
		pending, ok := h.sending[messageID]
		if !ok {
			done := make(chan struct{})
			if h.sending == nil {
				h.sending = map[string]chan struct{}{}
			}
			h.sending[messageID] = done
			h.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					h.mu.Lock()
					delete(h.sending, messageID)
					h.mu.Unlock()
					close(done)
				})
			}, nil
		}
		h.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (h *replayHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if req == nil || req.Message == nil || req.Message.ID == "" {
			yield(nil, a2a.ErrInvalidParams)
			return
		}
		unclaim, err := h.claim(ctx, req.Message.ID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer unclaim()
		id, ok, err := h.messages.MessageTask(ctx, req.Message.ID)
		if err != nil {
			yield(nil, err)
			return
		}
		if ok {
			unclaim()
			h.tel.MessageReplay()
			h.rejoin(ctx, id, yield)
			return
		}
		// Refuse an answer that does not decide the paused task's tool calls here, before the executor runs: an
		// executor error fails the task, and the session would wait on the calls for good.
		if req.Message.TaskID != "" {
			if err := h.checkAnswer(ctx, req.Message); err != nil {
				yield(nil, err)
				return
			}
		}
		// Keep the claim until the turn has a task, so a resend that races the first send finds it.
		recorded := false
		for ev, err := range h.RequestHandler.SendStreamingMessage(ctx, req) {
			if !recorded && err == nil && ev != nil {
				if id := ev.TaskInfo().TaskID; id != "" {
					if err := h.messages.RecordMessage(context.WithoutCancel(ctx), req.Message.ID, id); err != nil {
						h.log.Warn("record a message ID; a resend of the message will start another turn", "message", req.Message.ID, "task", id, "error", err)
					}
					unclaim()
					recorded = true
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// CancelTask cancels the task, and succeeds when the task has ended canceled although the cancel itself failed. That
// happens when the session's idle after the interrupt reaches the turn first: the turn ends canceled (Executor), and
// the request handler, which hands the cancel to the running turn, then finds that turn gone.
func (h *replayHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	task, err := h.RequestHandler.CancelTask(ctx, req)
	if err == nil || req == nil {
		return task, err
	}
	if settled, getErr := h.GetTask(ctx, &a2a.GetTaskRequest{ID: req.ID}); getErr == nil && settled.Status.State == a2a.TaskStateCanceled {
		return settled, nil
	}
	return task, err
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
