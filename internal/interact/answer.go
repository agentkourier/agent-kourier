// Package interact turns an agent's pause into a question in the thread (spec 8.4). The intake
// records a pause as an interaction and posts its prompt through the outbox, and when the same pause
// comes again after a crash it finishes what the crash left undone.
package interact

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// ProblemKind says why a question's answer cannot be used.
type ProblemKind int

// The kinds of problem.
const (
	// Unanswered: nothing usable was given. A missing block or action, null, "[]", "" and white
	// space are all this, and so is an input of the wrong type for its control.
	Unanswered ProblemKind = iota
	// Invalid: something was given that no honest press could hold: an option value that is not an
	// index of the question, a number out of range or not a number, or too many choices.
	Invalid
)

// Problem is a question whose answer cannot be used.
type Problem struct {
	Question int // index into the interaction's Questions
	Kind     ProblemKind
}

// Controls re-derives where each question's answer arrives in a press, from the stored interaction:
// the layout is deterministic, so the controls of the posted prompt are the controls of this call.
// A layout that changed under a pending prompt fails closed: its answers read as Unanswered.
func Controls(id string, in dialect.Interaction) ([]render.Control, error) {
	_, controls, err := render.PromptBlocks(id, in.Hint, in.Questions)
	return controls, err
}

// ErrNotAskUser is returned for an interaction that has no questions to answer: another kind of
// pause, or none. Nothing is read from a press for it.
var ErrNotAskUser = errors.New("interact: not an ask-user pause with questions")

// ExtractAnswer reads a press's input state (slack.Interaction.Inputs) as the answer to in, through
// the controls of its prompt. The answer is complete only if problems is empty. Option values are
// indices into a question's Choices and are mapped back to the stored choice, never taken as text;
// a numbers input holds 1-based positions separated by commas, semicolons or white space. A value is
// read only from the element type its question was rendered as. It returns ErrNotAskUser, and reads
// nothing, for an interaction that is not an ask-user pause with questions.
func ExtractAnswer(in dialect.Interaction, controls []render.Control, inputs map[string]map[string]slack.Input) (answer dialect.Answer, problems []Problem, err error) {
	if in.Kind != dialect.AskUser || len(in.Questions) == 0 {
		return dialect.Answer{}, nil, ErrNotAskUser
	}
	answer = dialect.Answer{RequestID: in.RequestID, Answers: make([][]string, len(in.Questions))}
	for i, q := range in.Questions {
		kind := Unanswered
		if len(controls) == len(in.Questions) { // a layout that does not match reads as unanswered
			vals, k, ok := extract(q, controls[i], inputs[controls[i].BlockID][controls[i].ActionID])
			if ok {
				answer.Answers[i] = vals
				continue
			}
			kind = k
		}
		problems = append(problems, Problem{i, kind})
	}
	return answer, problems, nil
}

// extract is one question's answer, or, with ok false, why there is none.
func extract(q dialect.Question, c render.Control, input slack.Input) (vals []string, why ProblemKind, ok bool) {
	if input.Type != c.Element {
		return nil, Unanswered, false
	}
	values := slices.DeleteFunc(slices.Clone(input.Values), func(v string) bool { return strings.TrimSpace(v) == "" })
	if len(values) == 0 {
		return nil, Unanswered, false
	}
	var out []string
	switch c.Kind {
	case render.ControlText, render.ControlNumbers:
		if len(values) > 1 { // a text input holds one value
			return nil, Invalid, false
		}
		if c.Kind == render.ControlText {
			out = []string{strings.TrimSpace(values[0])}
			break
		}
		for _, tok := range strings.FieldsFunc(values[0], func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' }) {
			n, ok := position(tok, 1, len(q.Choices)+1)
			if !ok {
				return nil, Invalid, false
			}
			out = append(out, q.Choices[n-1])
		}
		if len(out) == 0 {
			return nil, Unanswered, false
		}
	case render.ControlChoice, render.ControlChoices:
		for _, v := range values {
			n, ok := position(v, 0, len(q.Choices))
			if !ok {
				return nil, Invalid, false
			}
			out = append(out, q.Choices[n])
		}
	}
	out = dedupe(out)
	if len(out) > 1 && !q.Multiple {
		return nil, Invalid, false
	}
	return out, 0, true
}

// position parses s as a whole number in [lo, hi), written the one way courier writes it: no sign,
// no leading zero, no space.
func position(s string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && strconv.Itoa(n) == s && n >= lo && n < hi
}

func dedupe(vals []string) []string {
	var out []string
	for _, v := range vals {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
