package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// Stub drives the in-cluster stub agent's control surface, so a scenario can release the task that
// scenario-gated holds. The surface has no authentication, so this client is only for tests inside
// the cluster, and the stub's Service is never published.
type Stub struct {
	c   *client
	run string        // identifies this client, so names from another run never collide
	seq atomic.Uint64 // counts the names made
}

// NewStub returns a client for the stub agent at baseURL.
func NewStub(baseURL string) *Stub {
	b := make([]byte, 4)
	run := strconv.FormatInt(time.Now().UnixNano(), 36) // only if the system has no randomness
	if _, err := rand.Read(b); err == nil {
		run = hex.EncodeToString(b)
	}
	return &Stub{c: newClient(baseURL), run: run}
}

var notGateRune = regexp.MustCompile(`[^\w-]+`)

// Gate returns a gate name for a task of the test called name (t.Name()): the run, the name with
// anything a gate cannot hold replaced, and a count. The stub keeps a gate open for the rest of its
// life, so a name used twice would already be released: use each once, in the prompt that holds
// the task ("gate:" plus the name) and in the Release that frees it.
func (s *Stub) Gate(name string) string {
	return fmt.Sprintf("%s-%s-%d", s.run, notGateRune.ReplaceAllString(name, "-"), s.seq.Add(1))
}

// Release opens gate: the task held on it goes on, and one that reaches it later does not wait.
// It refuses a name the stub could not take, such as one not made by Gate.
func (s *Stub) Release(ctx context.Context, gate string) error {
	if !stubagent.ValidGate(gate) {
		return fmt.Errorf("e2e: %q is not a valid gate name; make one with Gate", gate)
	}
	return s.c.do(ctx, http.MethodPost, "/_control/release/"+gate, nil, nil)
}

// ArmSendGate holds the first runner reaching the stub's private before-send hook.
func (s *Stub) ArmSendGate(ctx context.Context) error {
	return s.c.do(ctx, http.MethodPost, "/_control/send-gate/arm", nil, nil)
}

// ReleaseSendGate lets the held runner continue to its agent's send fence.
func (s *Stub) ReleaseSendGate(ctx context.Context) error {
	return s.c.do(ctx, http.MethodPost, "/_control/send-gate/release", nil, nil)
}

// SendGate reads the held message and the counts of actual A2A send attempts.
func (s *Stub) SendGate(ctx context.Context) (stubagent.SendGateState, error) {
	var state stubagent.SendGateState
	err := s.c.do(ctx, http.MethodGet, "/_control/send-gate", nil, &state)
	return state, err
}

// AgentTask is one task of a stub agent, as its ListTasks shows it: its ID, its context, and the text of the message that started it,
// which is the prompt Agent Kourier sent for the turn.
type AgentTask struct {
	ID, ContextID, Prompt string
}

// listPage is the most tasks Tasks asks for at once: the stub's largest page.
const listPage = 100

// listAnswer is the stub's answer to a ListTasks: JSON-RPC, in which an error is HTTP 200 too.
type listAnswer struct {
	Result *struct {
		Tasks []struct {
			ID        string `json:"id"`
			ContextID string `json:"contextId"`
			History   []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
		} `json:"tasks"`
		NextPageToken string `json:"nextPageToken"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Tasks lists the tasks of the stub's agent namespace/name that the identity of bearer owns (the stub's owner is the email of the JWT,
// as kagent's is), oldest first, every page of them. bearer is the token a Binding sends, so that a scenario whose Binding has an
// identity of its own lists that Binding's tasks alone. An error in the JSON-RPC answer is an error here.
func (s *Stub) Tasks(ctx context.Context, namespace, name, bearer string) ([]AgentTask, error) {
	c := newClient(s.c.base)
	c.limit = maxLog
	c.header = http.Header{"Authorization": {"Bearer " + bearer}}
	var out []AgentTask
	for token := ""; ; {
		params := map[string]any{"pageSize": listPage}
		if token != "" {
			params["pageToken"] = token
		}
		var answer listAnswer
		rpc := map[string]any{"jsonrpc": "2.0", "id": "1", "method": "ListTasks", "params": params}
		if err := c.do(ctx, http.MethodPost, "/agents/"+namespace+"/"+name, rpc, &answer); err != nil {
			return nil, err
		}
		switch {
		case answer.Error != nil:
			return nil, fmt.Errorf("e2e: ListTasks of %s/%s: %d %s", namespace, name, answer.Error.Code, answer.Error.Message)
		case answer.Result == nil:
			return nil, fmt.Errorf("e2e: ListTasks of %s/%s answered no result", namespace, name)
		}
		for _, t := range answer.Result.Tasks {
			task := AgentTask{ID: t.ID, ContextID: t.ContextID}
			if len(t.History) > 0 {
				for _, p := range t.History[0].Parts {
					task.Prompt += p.Text
				}
			}
			out = append(out, task)
		}
		if token = answer.Result.NextPageToken; token == "" {
			return out, nil
		}
	}
}
