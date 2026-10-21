// Package hitlagent is a small A2A 1.0 agent that asks a person to approve one tool call through kagent's
// human-in-the-loop extension, written from Agent Kourier's public agent contract alone. It serves JSON-RPC with
// streaming at "/" and its card at "/.well-known/agent-card.json", and accepts any caller.
package hitlagent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// TriggerPhrase in a new turn's text makes the agent pause for approval of add_numbers.
const TriggerPhrase = "use the approval tool"

// DefaultAgentURL is the interface URL the card lists when none is configured.
const DefaultAgentURL = "http://hitlagent.e2e.svc.cluster.local:8080/"

// storeUser is the one owner of every task. The agent has no auth, but the SDK's in-memory store refuses
// ListTasks for an empty user name, and the contract needs ListTasks to work.
const storeUser = "anonymous"

// NewCard returns the agent card: JSON-RPC at agentURL, streaming, and the HITL extension listed as optional.
func NewCard(agentURL string) *a2a.AgentCard {
	if agentURL == "" {
		agentURL = DefaultAgentURL
	}
	return &a2a.AgentCard{
		Name:        "hitlagent",
		Description: "Test agent that asks a person to approve an add_numbers tool call before it runs it.",
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(agentURL, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		Capabilities: a2a.AgentCapabilities{
			Streaming: true,
			Extensions: []a2a.AgentExtension{{
				URI:         ExtensionURI,
				Description: "Pauses for tool approvals with kagent's human-in-the-loop payload.",
				Required:    false,
			}},
		},
		Skills: []a2a.AgentSkill{{
			ID:          "add_numbers",
			Name:        "Add numbers, with approval",
			Description: "Asks for approval, then adds 3 and 5.",
			Tags:        []string{"hitl", "approval"},
			Examples:    []string{"please " + TriggerPhrase},
		}},
	}
}

// NewHandler returns the agent's HTTP handler: the card at a2asrv.WellKnownAgentCardPath and JSON-RPC at "/".
func NewHandler(agentURL string) (http.Handler, error) {
	card := NewCard(agentURL)
	// The enforcer gets the card's capabilities alone: given the whole card it would also check message parts
	// against the input modes, and the card's "text" is not a media type.
	enforcer, err := a2asrv.NewAgentCardEnforcer(&a2a.AgentCard{Capabilities: card.Capabilities})
	if err != nil {
		return nil, fmt.Errorf("agent card enforcer: %w", err)
	}
	ag := &agent{
		store: taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
			Authenticator: func(context.Context) (string, error) { return storeUser, nil },
		}),
		turns: make(map[string]a2a.TaskID),
	}
	reqHandler := a2asrv.NewHandler(ag,
		a2asrv.WithTaskStore(ag.store),
		a2asrv.WithCallInterceptors(enforcer, ag),
	)

	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/{$}", echoExtension(a2asrv.NewJSONRPCHandler(reqHandler)))
	return mux, nil
}

// echoExtension names the HITL extension in the response's A2A-Extensions header when the request asked for it.
// The SDK's JSON-RPC transport does not echo activated extensions, and a stream writes its headers before the
// request reaches the agent, so this is done in front of the transport.
func echoExtension(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requested(r.Header.Values(a2a.SvcParamExtensions)) {
			w.Header().Set(a2a.SvcParamExtensions, ExtensionURI)
		}
		next.ServeHTTP(w, r)
	})
}

// requested reports whether A2A-Extensions header values, comma-separated or repeated, name ExtensionURI.
func requested(values []string) bool {
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(item) == ExtensionURI {
				return true
			}
		}
	}
	return false
}

// agent implements the executor and a call interceptor that checks answers before they reach the executor.
type agent struct {
	a2asrv.PassthroughCallInterceptor

	store taskstore.Store

	mu    sync.Mutex
	turns map[string]a2a.TaskID // messageId of a new turn -> the task it started
}

var (
	_ a2asrv.AgentExecutor   = (*agent)(nil)
	_ a2asrv.CallInterceptor = (*agent)(nil)
)

// Before runs ahead of every call. For a send it does what the contract asks and the SDK does not: a message whose
// messageId was seen before (a new turn, or an answer the task already holds) gets the task's snapshot instead of
// running again, and an answer to a paused approval that does not decide it is refused with JSON-RPC -32603 while
// the task stays paused. Without the replay check, an answer sent again to its now-completed task would get the
// SDK's "task in a terminal state", which is -32004, and Agent Kourier reads -32004 as busy and sends again.
func (a *agent) Before(ctx context.Context, _ *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	send, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || send.Message == nil {
		return ctx, nil, nil
	}
	msg := send.Message

	if msg.TaskID == "" {
		if tid, seen := a.turnTask(msg.ID); seen {
			stored, err := a.store.Get(ctx, tid)
			if err == nil {
				return ctx, a2a.SendMessageResult(stored.Task), nil
			}
		}
		return ctx, nil, nil
	}

	stored, err := a.store.Get(ctx, msg.TaskID)
	if err != nil {
		return ctx, nil, nil // the handler reports the missing task
	}
	if slices.ContainsFunc(stored.Task.History, func(m *a2a.Message) bool { return m.ID == msg.ID }) {
		return ctx, a2a.SendMessageResult(stored.Task), nil
	}
	pending, ok := pendingApproval(stored.Task)
	if !ok {
		return ctx, nil, nil
	}
	if _, err := parseApprovalResponse(pending, msg); err != nil {
		return ctx, nil, fmt.Errorf("%w: %w", a2a.ErrInternalError, err)
	}
	return ctx, nil, nil
}

// Execute handles a new turn (pause or greet) or the answer to a paused approval.
func (a *agent) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			a.newTurn(execCtx, yield)
			return
		}
		a.answer(execCtx, yield)
	}
}

// Cancel moves the task to canceled.
func (a *agent) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func (a *agent) newTurn(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	msg := execCtx.Message
	a.rememberTurn(msg.ID, execCtx.TaskID)

	if !yield(a2a.NewSubmittedTask(execCtx, msg), nil) {
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
		return
	}

	text := messageText(msg)
	if strings.Contains(strings.ToLower(text), TriggerPhrase) {
		pause, err := approvalPause(execCtx)
		if err != nil {
			yield(failed(execCtx, err), nil)
			return
		}
		yield(pause, nil)
		return
	}

	greeting := "Hello from hitlagent. You said: " + strings.Join(strings.Fields(text), " ")
	if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(greeting)), nil) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

func (a *agent) answer(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	pending, ok := pendingApproval(execCtx.StoredTask)
	if !ok {
		// Only approval pauses exist; a send to a task in any other live state is not something this agent takes.
		yield(failed(execCtx, errors.New("task is not waiting for an approval")), nil)
		return
	}
	decisions, err := parseApprovalResponse(pending, execCtx.Message)
	if err != nil {
		// Before refuses these with -32603; this is only reached if that check was bypassed.
		yield(failed(execCtx, err), nil)
		return
	}

	if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
		return
	}
	lines := make([]string, 0, len(decisions))
	for _, d := range decisions {
		lines = append(lines, outcome(d))
	}
	if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(strings.Join(lines, "\n"))), nil) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

// approvalPause builds the input-required status update that asks for approval of add_numbers(3, 5).
func approvalPause(execCtx *a2asrv.ExecutorContext) (*a2a.TaskStatusUpdateEvent, error) {
	req := ToolApprovalRequest{
		Type: typeToolApprovalRequest,
		Hint: "Approve add_numbers to add 3 and 5.",
		Tools: []ToolCall{{
			ID:     "approval-" + string(execCtx.TaskID),
			CallID: "call-" + string(execCtx.TaskID),
			Name:   "add_numbers",
			Args:   map[string]any{"a": 3, "b": 5},
		}},
	}
	payload, err := toMetadataValue(req)
	if err != nil {
		return nil, err
	}
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx,
		a2a.NewTextPart("Waiting for approval to run add_numbers with a=3, b=5."))
	msg.Extensions = []string{ExtensionURI}
	msg.Metadata = map[string]any{ExtensionURI: payload}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, msg), nil
}

// outcome runs an approved tool, or reports a denied one, as one line of the artifact.
func outcome(d decision) string {
	if !d.approved {
		reason := d.reason
		if reason == "" {
			reason = "no reason given"
		}
		return "Denied: " + reason
	}
	a, errA := number(d.tool.Args["a"])
	b, errB := number(d.tool.Args["b"])
	if err := errors.Join(errA, errB); err != nil {
		return fmt.Sprintf("Approved, but %s could not run: %v", d.tool.Name, err)
	}
	return fmt.Sprintf("Approved: %s + %s = %s.", formatNumber(a), formatNumber(b), formatNumber(a+b))
}

func number(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("argument %v is not a number", v)
	}
}

func formatNumber(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func failed(execCtx *a2asrv.ExecutorContext, err error) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(err.Error()))
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg)
}

func messageText(msg *a2a.Message) string {
	var parts []string
	for _, p := range msg.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func (a *agent) rememberTurn(messageID string, tid a2a.TaskID) {
	if messageID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.turns[messageID]; !ok {
		a.turns[messageID] = tid
	}
}

func (a *agent) turnTask(messageID string) (a2a.TaskID, bool) {
	if messageID == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tid, ok := a.turns[messageID]
	return tid, ok
}
