package interact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// answeredWord is what a free-text answer is shown as once the prompt is replaced. What the person typed may
// be a secret, and the channel keeps what it is shown (the owner's decision), so only that there is an answer
// is said, in Agent Kourier's own word.
const answeredWord = "(answered)"

// These builders make the messages of an interaction in the chat-neutral model (package chat): the prompt that
// asks, and the ending that takes its place. They carry what the agent wrote as literal text and say nothing of
// controls, layout or limits: each adapter lays the prompt out for its platform, makes the text inert, cuts it
// to what the platform shows, and says whether it fits (Platform.Fits).

// promptOf is the neutral prompt of a pause: for a question, the agent's hint, its questions, the deadline, and the
// sub-agent that raised it, if one did; for a tool approval, the hint, the tool calls, the deadline and the
// sub-agent. id is what a press carries back.
//
// A question is shown as the agent wrote it. An approval is shown as it is stored (storable): the arguments are
// the person's only evidence of what a tool will do, but they are also where a secret sits, and the channel keeps
// what it is shown, so what is shown is never more than what is kept. The decision is still made from the tool IDs,
// which that leaves as they were.
//
// Every tool call is shown as it will run, except that secrets are replaced by the redaction's marker, invisible format
// characters in values are not shown, line ends are shown as LF, and a number is shown as the nearest float64. Anything
// else that cannot be shown is an error wrapping dialect.ErrUnusablePause (exactTools), for the caller to refuse: it is
// checked here as well as by Paused, so that no caller can make a prompt that shows something else from a raw pause. The prompt
// of an approval that is already stored is made by toolsOf instead (Intake.post): it is not checked or redacted again. The hint and the
// sub-agent's name are context and may be shown cut.
func promptOf(id string, in dialect.Interaction, deadline time.Time) (chat.Prompt, error) {
	switch in.Kind {
	case dialect.AskUser:
		return questionsOf(id, in, deadline), nil
	case dialect.ToolApproval:
		if err := exactTools(in.Tools); err != nil {
			return chat.Prompt{}, err
		}
		return toolsOf(id, storable(in), deadline)
	}
	return chat.Prompt{}, fmt.Errorf("interact: no prompt is made for a %q pause", in.Kind)
}

// questionsOf is what a prompt and its ending say of the question: the hint, the questions and the sub-agent.
func questionsOf(id string, in dialect.Interaction, deadline time.Time) chat.Prompt {
	p := chat.Prompt{ID: id, Hint: in.Hint, Deadline: deadline}
	for _, q := range in.Questions {
		p.Questions = append(p.Questions, chat.Question{Text: q.Text, Choices: q.Choices, Multiple: q.Multiple})
	}
	if in.Nested != nil {
		p.Subagent = in.Nested.Subagent
	}
	return p
}

// toolsOf is what a prompt and its ending say of an approval: the hint, one tool for each tool call in the order the
// agent listed them, and the sub-agent. in must be as it is stored (storable): the secrets are taken out of it
// by the caller, and are not taken out again here (redacting twice would cut a cut text again).
func toolsOf(id string, in dialect.Interaction, deadline time.Time) (chat.Prompt, error) {
	p := chat.Prompt{ID: id, Hint: in.Hint, Deadline: deadline}
	for _, t := range in.Tools {
		args, err := argsText(t.Args)
		if err != nil {
			return chat.Prompt{}, fmt.Errorf("interact: the arguments of tool %s: %w", t.ID, err)
		}
		p.Tools = append(p.Tools, chat.Tool{Name: t.Name, Args: args})
	}
	if in.Nested != nil {
		p.Subagent = in.Nested.Subagent
	}
	return p, nil
}

// argsText is a tool's arguments as the text a person reads: indented JSON, whose object keys encoding/json sorts,
// so the same pause always renders the same text. The characters <, > and & are written as they are and not as
// \u003c escapes, which would show a person other text than the agent sent. A tool with no arguments (a nil or
// an empty map) has none to show, and its Args is "".
func argsText(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(args); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil // Encode ends the value with a newline
}

// endingOf is the neutral ending of a prompt: how it ended, who decided (by, as Agent Kourier verified it, or ""),
// and when. For an answered question, answers is what was answered to each question, in order; the ending shows
// the chosen choices and, for a free-text question, only that it was answered. The prompt of an ending is
// the question as asked, which has no deadline of its own to show.
//
// An approval's ending shows its tools, hint and sub-agent as in, which must be the stored request: its secrets are
// out already, and what it holds is what the prompt showed. When the approval timed out or was cancelled it needs
// nothing more. An answered approval needs a verdict for each tool, which only the answer holds (approvalEnding), so
// here it is an error and not an ending that says the wrong thing.
func endingOf(outcome chat.Outcome, by string, at time.Time, in dialect.Interaction, answers [][]string) (chat.Ending, error) {
	if in.Kind == dialect.ToolApproval {
		if outcome == chat.Answered {
			return chat.Ending{}, errors.New("interact: an answered tool approval has verdicts, which endingOf is not given: use approvalEnding")
		}
		p, err := toolsOf("", in, time.Time{})
		if err != nil {
			return chat.Ending{}, err
		}
		return chat.Ending{Outcome: outcome, By: by, At: at, Prompt: p}, nil
	}
	e := chat.Ending{Outcome: outcome, By: by, At: at, Prompt: questionsOf("", in, time.Time{})}
	if outcome != chat.Answered {
		return e, nil
	}
	e.Answers = make([][]string, len(in.Questions))
	for i, q := range in.Questions {
		var given []string
		if i < len(answers) {
			given = answers[i]
		}
		switch {
		case len(given) == 0: // no answer: the adapter says so
		case len(q.Choices) == 0:
			e.Answers[i] = []string{answeredWord}
		default:
			e.Answers[i] = given
		}
	}
	return e, nil
}

// approvalEnding is the neutral ending of an answered tool approval: its tools as in (the stored request) shows them, and
// the verdict of each, in the order of the tools and from the decisions by tool ID. The decisions must be exactly one for each
// tool and for no other: an answer that does not match the request is an error, never an ending built from a guess. A
// decision by the broker's own policy (auto) has no person to name, and so no By.
func approvalEnding(by string, at time.Time, in dialect.Interaction, decisions []dialect.Decision) (chat.Ending, error) {
	p, err := toolsOf("", in, time.Time{})
	if err != nil {
		return chat.Ending{}, err
	}
	if len(decisions) != len(in.Tools) {
		return chat.Ending{}, fmt.Errorf("interact: %d decisions for %d tools", len(decisions), len(in.Tools))
	}
	// One decision for each tool, by ID: with as many decisions as tools, a tool decided twice leaves another with none.
	byID := make(map[string]dialect.Decision, len(decisions))
	for _, d := range decisions {
		byID[d.ToolID] = d
	}
	verdicts := make([]chat.ToolVerdict, len(in.Tools))
	for i, t := range in.Tools {
		d, ok := byID[t.ID]
		if !ok {
			return chat.Ending{}, fmt.Errorf("interact: no decision for tool %d", i+1)
		}
		verdicts[i] = chat.ToolDenied
		if d.Approved {
			verdicts[i] = chat.ToolApproved
		}
	}
	if by == autoDecider {
		by = ""
	}
	e := chat.Ending{Outcome: chat.Answered, By: by, At: at, Prompt: p, Verdicts: verdicts}
	return e, e.Validate()
}
