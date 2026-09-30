package kagentv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// The HITL extension's payload types, as sent in a paused task's status message metadata.
const (
	askUserRequest      = "ask_user_request"
	toolApprovalRequest = "tool_approval_request"
)

// wireTool, wireQuestion, wireNested and hitlRequest mirror kagent's request payloads
// (go/api/a2a/hitl.go). Measured at alpha5 (WP-02): the ask-user and tool-approval
// requests, in test/fixtures/kagent/askuser and approval. Source only, never captured:
// the nested object, which either request kind may carry.
type wireTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

type wireQuestion struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Multiple bool     `json:"multiple"`
}

type wireNested struct {
	SubagentName string     `json:"subagent_name"`
	TaskID       string     `json:"task_id"`
	ContextID    string     `json:"context_id"`
	Tools        []wireTool `json:"tools"`
}

type hitlRequest struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Hint      string         `json:"hint"`
	Questions []wireQuestion `json:"questions"`
	Tools     []wireTool     `json:"tools"`
	Nested    *wireNested    `json:"nested"`
}

// The most a pause may ask, so that a broken or hostile agent cannot make courier render
// an unbounded prompt. Slack's own select menus stop at 100 options.
const (
	maxQuestions = 20
	maxChoices   = 100
)

// unusable returns an error that wraps dialect.ErrUnusablePause and says why. The reason
// is fixed text, never the agent's: it goes into a thread, and the agent is untrusted.
func unusable(reason string) error {
	return fmt.Errorf("%w: %s", dialect.ErrUnusablePause, reason)
}

// ParseInteraction returns the interaction a task paused on. A task that is not
// input-required has none, and gives nil, nil. An input-required task whose HITL request
// cannot be answered gives an error wrapping dialect.ErrUnusablePause, because kagent-v1
// never falls back to free text: no payload (the request lacked the extension header,
// which Connect sends), an unknown type, no id, no questions or tools, a tool without an
// id, more than 20 questions or 100 choices, or a nested request that fails the checks
// below.
//
// A nested request (a sub-agent's) is usable only when kagent's resume code can correlate
// the answer (v1.0.0-alpha5 go/adk/pkg/a2a/hitl.go, source only, never captured). It
// needs the sub-agent's name and task. A nested ask-user needs exactly one child ask_user
// tool, whose id, not the top-level one, is what the answer echoes
// (processNestedAskUser). A nested tool approval decides only the nested tools, with no
// fallback to the top-level ones, and needs exactly one top-level tool
// (processNestedApproval). That tool's id must be non-empty because it becomes the parent
// FunctionResponse id, so an empty one could never resume the task. This is stricter than
// kagent's own check, but it cannot reject a pause that could be resumed.
func (*Dialect) ParseInteraction(task a2a.Task) (*dialect.Interaction, error) {
	if task.Status.State != a2a.TaskStateInputRequired {
		return nil, nil // no structured pause is the documented "nothing to parse", not a failure
	}
	msg := task.Status.Message
	if msg == nil || msg.Metadata[HITLExtension] == nil {
		return nil, unusable("no HITL payload (was the extension header sent?)")
	}
	raw, err := json.Marshal(msg.Metadata[HITLExtension])
	var req hitlRequest
	if err != nil || json.Unmarshal(raw, &req) != nil {
		return nil, unusable("malformed HITL payload")
	}
	in := &dialect.Interaction{TaskID: task.ID, ContextID: task.ContextID, Hint: req.Hint}
	if n := req.Nested; n != nil {
		if n.TaskID == "" || n.SubagentName == "" {
			return nil, unusable("nested request has no sub-agent name or task")
		}
		in.Nested = &dialect.Nested{Subagent: n.SubagentName, TaskID: n.TaskID, ContextID: n.ContextID}
	}
	switch req.Type {
	case askUserRequest:
		err = parseAskUser(req, in)
	case toolApprovalRequest:
		err = parseToolApproval(req, in)
	default:
		err = unusable("unknown request type")
	}
	if err != nil {
		return nil, err
	}
	return in, nil
}

// parseAskUser fills in from an ask-user request.
func parseAskUser(req hitlRequest, in *dialect.Interaction) error {
	if req.ID == "" {
		return unusable("ask-user request has no id")
	}
	in.Kind, in.RequestID = dialect.AskUser, req.ID
	if n := req.Nested; n != nil {
		switch {
		case len(n.Tools) != 1 || n.Tools[0].Name != "ask_user":
			return unusable("nested ask-user needs exactly one ask_user tool")
		case n.Tools[0].ID == "":
			return unusable("nested ask-user tool has no id")
		}
		in.RequestID = n.Tools[0].ID
	}
	switch {
	case len(req.Questions) == 0:
		return unusable("ask-user request has no questions")
	case len(req.Questions) > maxQuestions:
		return unusable(fmt.Sprintf("ask-user request has more than %d questions", maxQuestions))
	}
	for _, q := range req.Questions {
		if len(q.Choices) > maxChoices {
			return unusable(fmt.Sprintf("a question has more than %d choices", maxChoices))
		}
		in.Questions = append(in.Questions, dialect.Question{Text: q.Question, Choices: q.Choices, Multiple: q.Multiple})
	}
	return nil
}

// parseToolApproval fills in from a tool-approval request.
func parseToolApproval(req hitlRequest, in *dialect.Interaction) error {
	in.Kind = dialect.ToolApproval
	tools := req.Tools
	if n := req.Nested; n != nil {
		if len(req.Tools) != 1 || req.Tools[0].ID == "" {
			return unusable("nested tool approval needs exactly one outer tool with an id")
		}
		tools = n.Tools
	}
	if len(tools) == 0 {
		return unusable("tool approval has no tools")
	}
	for _, w := range tools {
		if w.ID == "" {
			return unusable("tool approval has a tool without an id")
		}
		in.Tools = append(in.Tools, dialect.Tool{ID: w.ID, CallID: w.CallID, Name: w.Name, Args: w.Args})
	}
	return nil
}

// The response payloads are plain maps, as kagent's AttachHitlExtension stores them in
// message metadata. Their shapes are what kagent accepts as a reply (measured at alpha5,
// WP-02). A nested reply has the same shapes (kagent source); what differs is the id an
// ask-user reply echoes, which Interaction.RequestID already holds.

// BuildResponse returns the user message that answers in: the task and context of the
// pause, the HITL extension, and the payload kagent expects. It checks the answer first,
// because kagent accepts an incomplete or off-choice answer, and wedges the task on an
// empty answer list or a wrong request id (spec 6.3, R17). Every question needs an answer
// that is one of its choices (when it has any), and only one unless it is multi-select;
// every tool needs exactly one decision; the request id must be in's. Otherwise it
// returns an error and no message. The A2A-Extensions header comes from Connect.
func (*Dialect) BuildResponse(in dialect.Interaction, answer dialect.Answer) (a2a.Message, error) {
	if in.TaskID == "" {
		return a2a.Message{}, errors.New("interaction has no task to answer")
	}
	var payload map[string]any
	var text string
	var err error
	switch in.Kind {
	case dialect.AskUser:
		payload, text, err = askUserAnswer(in, answer)
	case dialect.ToolApproval:
		payload, text, err = toolApprovalAnswer(in, answer)
	default:
		err = fmt.Errorf("unknown interaction kind %q", in.Kind)
	}
	if err != nil {
		return a2a.Message{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = in.TaskID, in.ContextID
	msg.Extensions = []string{HITLExtension}
	msg.Metadata = map[string]any{HITLExtension: payload}
	return *msg, nil
}

func askUserAnswer(in dialect.Interaction, a dialect.Answer) (map[string]any, string, error) {
	// An Interaction is reloaded from the store, so its own checks come first: no
	// questions would build answers: null, which wedges kagent.
	switch {
	case in.RequestID == "":
		return nil, "", errors.New("interaction has no request id")
	case len(in.Questions) == 0:
		return nil, "", errors.New("interaction has no questions")
	case a.RequestID != in.RequestID:
		return nil, "", errors.New("answer is for a different request")
	case len(a.Decisions) > 0:
		return nil, "", errors.New("tool decisions cannot answer questions")
	case len(a.Answers) != len(in.Questions):
		return nil, "", fmt.Errorf("%d of %d questions answered", len(a.Answers), len(in.Questions))
	}
	answers := make([]any, 0, len(in.Questions))
	flat := make([]string, 0, len(in.Questions))
	for i, q := range in.Questions {
		if err := checkAnswer(q, a.Answers[i]); err != nil {
			return nil, "", fmt.Errorf("question %d: %w", i+1, err)
		}
		answers = append(answers, map[string]any{"answer": a.Answers[i]})
		flat = append(flat, strings.Join(a.Answers[i], ", "))
	}
	return map[string]any{"type": "ask_user_response", "id": in.RequestID, "answers": answers}, strings.Join(flat, "; "), nil
}

// checkAnswer reports why vals does not answer q.
func checkAnswer(q dialect.Question, vals []string) error {
	if len(vals) == 0 {
		return errors.New("not answered")
	}
	if len(vals) > 1 && !q.Multiple {
		return errors.New("only one answer is allowed")
	}
	for i, v := range vals {
		switch {
		case strings.TrimSpace(v) == "":
			return errors.New("blank answer")
		case len(q.Choices) > 0 && !slices.Contains(q.Choices, v):
			return fmt.Errorf("%q is not one of the choices", v)
		case slices.Contains(vals[:i], v):
			return fmt.Errorf("%q is chosen twice", v)
		}
	}
	return nil
}

func toolApprovalAnswer(in dialect.Interaction, a dialect.Answer) (map[string]any, string, error) {
	switch {
	case len(in.Tools) == 0: // approvals: null is read by kagent as plain text, and the tool does not run
		return nil, "", errors.New("interaction has no tool calls")
	case len(a.Answers) > 0:
		return nil, "", errors.New("question answers cannot decide tool calls")
	case len(a.Decisions) != len(in.Tools):
		return nil, "", fmt.Errorf("%d of %d tool calls decided", len(a.Decisions), len(in.Tools))
	}
	byID := make(map[string]dialect.Decision, len(a.Decisions))
	for _, d := range a.Decisions {
		if !slices.ContainsFunc(in.Tools, func(t dialect.Tool) bool { return t.ID == d.ToolID }) {
			return nil, "", errors.New("decision for a tool call that is not pending")
		}
		if _, twice := byID[d.ToolID]; twice {
			return nil, "", errors.New("a tool call is decided twice")
		}
		byID[d.ToolID] = d
	}
	approvals, text := make([]any, 0, len(in.Tools)), "approved"
	for _, t := range in.Tools { // in request order, one decision each
		d := byID[t.ID]
		w := map[string]any{"id": t.ID, "approved": d.Approved}
		if !d.Approved {
			text = "rejected"
			if d.Reason != "" {
				w["rejection_reason"] = d.Reason
			}
		}
		approvals = append(approvals, w)
	}
	return map[string]any{"type": "tool_approval_response", "approvals": approvals}, text, nil
}
