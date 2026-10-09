package interact

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// ProblemKind says why a question's answer cannot be used.
type ProblemKind int

// The kinds of problem.
const (
	// Unanswered: nothing usable was given. A missing answer, no values, "" and white space are all this.
	Unanswered ProblemKind = iota
	// Invalid: something was given that no honest press could hold: an option value that is not an
	// index of the question, or too many choices.
	Invalid
)

// Problem is a question whose answer cannot be used.
type Problem struct {
	Question int // index into the interaction's Questions
	Kind     ProblemKind
}

// ErrNotAskUser is returned for an interaction that has no questions to answer: another kind of
// pause, or none. Nothing is read from a press for it.
var ErrNotAskUser = errors.New("interact: not an ask-user pause with questions")

// ExtractAnswer reads a press's answers (chat.Interaction.Answers, which the platform's adapter decoded from
// its controls, one for each question answered) as the answer to in. The answer is complete only if
// problems is empty. A choice question's values are indices into its Choices and are mapped back to the
// stored choice, never taken as text; a question with no choices is free text. It returns ErrNotAskUser,
// and reads nothing, for an interaction that is not an ask-user pause with questions.
//
// An answer that names a question the prompt does not have (an index below zero, or past the last
// question) is no honest press: the adapter made the controls from this prompt. The press is refused as
// invalid, every question of it, and nothing in it is read. So is a question answered twice: which of the
// two is meant cannot be told.
func ExtractAnswer(in dialect.Interaction, answers []chat.Answer) (answer dialect.Answer, problems []Problem, err error) {
	if in.Kind != dialect.AskUser || len(in.Questions) == 0 {
		return dialect.Answer{}, nil, ErrNotAskUser
	}
	answer = dialect.Answer{RequestID: in.RequestID, Answers: make([][]string, len(in.Questions))}
	byQuestion := make([][]chat.Answer, len(in.Questions))
	stray := false
	for _, a := range answers {
		if a.Question < 0 || a.Question >= len(in.Questions) {
			stray = true
			continue
		}
		byQuestion[a.Question] = append(byQuestion[a.Question], a)
	}
	for i, q := range in.Questions {
		switch {
		case stray || len(byQuestion[i]) > 1:
			problems = append(problems, Problem{i, Invalid})
		case len(byQuestion[i]) == 0:
			problems = append(problems, Problem{i, Unanswered})
		default:
			vals, kind, ok := extract(q, byQuestion[i][0].Values)
			if ok {
				answer.Answers[i] = vals
				continue
			}
			problems = append(problems, Problem{i, kind})
		}
	}
	return answer, problems, nil
}

// extract is one question's answer, or, with ok false, why there is none.
func extract(q dialect.Question, given []string) (vals []string, why ProblemKind, ok bool) {
	values := slices.DeleteFunc(slices.Clone(given), func(v string) bool { return strings.TrimSpace(v) == "" })
	if len(values) == 0 {
		return nil, Unanswered, false
	}
	if len(q.Choices) == 0 { // free text: one value
		if len(values) > 1 {
			return nil, Invalid, false
		}
		return []string{strings.TrimSpace(values[0])}, 0, true
	}
	var out []string
	for _, v := range values {
		n, ok := position(v, 0, len(q.Choices))
		if !ok {
			return nil, Invalid, false
		}
		out = append(out, q.Choices[n])
	}
	out = dedupe(out)
	if len(out) > 1 && !q.Multiple {
		return nil, Invalid, false
	}
	return out, 0, true
}

// position parses s as a whole number in [lo, hi), written the one way Agent Kourier writes it: no sign,
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
