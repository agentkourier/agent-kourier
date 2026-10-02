package interact

import (
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

const hostileAgentText = "<!channel> <@U123> <https://x|y> & [bank](https://evil.example) end"

func askUser(questions ...dialect.Question) dialect.Interaction {
	return dialect.Interaction{Kind: dialect.AskUser, TaskID: "t1", Hint: hostileAgentText, RequestID: "r1", Questions: questions}
}

var askBy = time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

// A prompt carries what the agent wrote as it was written: literal, with no escaping and no layout. The
// adapter makes it inert, which the conformance suite proves of it.
func TestPromptOfCarriesTheQuestionsLiterally(t *testing.T) {
	in := askUser(
		dialect.Question{Text: hostileAgentText},
		dialect.Question{Text: "Which?", Choices: []string{"a", hostileAgentText}, Multiple: true},
	)
	in.Nested = &dialect.Nested{Subagent: "planner"}
	got, err := promptOf("ix-1", in, askBy)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.Prompt{
		ID: "ix-1", Hint: hostileAgentText, Deadline: askBy, Subagent: "planner",
		Questions: []chat.Question{{Text: hostileAgentText}, {Text: "Which?", Choices: []string{"a", hostileAgentText}, Multiple: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("promptOf = %+v, want %+v", got, want)
	}
	if got.Validate() != nil {
		t.Errorf("the prompt is not valid: %v", got.Validate())
	}
}

func TestPromptOfRefusesWhatIsNotAnAskUserPause(t *testing.T) {
	tool := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "t1", RequestID: "r1", Questions: []dialect.Question{{Text: "q"}}, Tools: []dialect.Tool{{ID: "c1", Name: "rm"}}}
	if _, err := promptOf("ix-1", tool, askBy); err == nil {
		t.Error("a tool approval was made into a prompt")
	}
	// A prompt with no question is the platform's to refuse (chat.Prompt.Validate), and promptOf makes it as it is.
	if p, err := promptOf("ix-1", askUser(), askBy); err != nil || p.Validate() == nil {
		t.Errorf("a pause with no question = %+v, %v; want a prompt that does not validate", p, err)
	}
}

// An ending shows the choices that were chosen, says only that a free-text question was answered, and
// leaves a question that has no answer without one.
func TestEndingOfShowsChoicesAndNeverFreeText(t *testing.T) {
	in := askUser(
		dialect.Question{Text: "Why?"},
		dialect.Question{Text: "Which?", Choices: []string{"a", "b"}},
		dialect.Question{Text: "Some?", Choices: []string{"a", "b", "c"}, Multiple: true},
		dialect.Question{Text: "Skipped?", Choices: []string{"x"}},
		dialect.Question{Text: "Unasked?"},
	)
	answers := [][]string{{"my password is hunter2"}, {"b"}, {"a", "c"}, {}}
	got := endingOf(chat.Answered, "UALICE1", askBy, in, answers)
	want := [][]string{{answeredWord}, {"b"}, {"a", "c"}, nil, nil}
	if !reflect.DeepEqual(got.Answers, want) {
		t.Errorf("answers = %q, want %q", got.Answers, want)
	}
	if got.Outcome != chat.Answered || got.By != "UALICE1" || !got.At.Equal(askBy) || len(got.Prompt.Questions) != 5 || got.Prompt.Hint != hostileAgentText {
		t.Errorf("ending = %+v", got)
	}
	if !got.Prompt.Deadline.IsZero() || got.Prompt.ID != "" {
		t.Errorf("an ending's prompt is the question as asked: no ID, no deadline; got %+v", got.Prompt)
	}
	if got.Validate() != nil {
		t.Errorf("the ending is not valid: %v", got.Validate())
	}
}

// Only an answered prompt shows answers: a timed-out or cancelled one has none, even if it is given some.
func TestEndingOfOtherOutcomesHasNoAnswers(t *testing.T) {
	in := askUser(dialect.Question{Text: "Which?", Choices: []string{"a", "b"}})
	in.Nested = &dialect.Nested{Subagent: "planner"}
	for _, outcome := range []chat.Outcome{chat.TimedOut, chat.Cancelled} {
		got := endingOf(outcome, "", askBy, in, [][]string{{"a"}})
		if got.Answers != nil || got.Outcome != outcome || got.Prompt.Subagent != "planner" {
			t.Errorf("%s: ending = %+v; want no answers, and the sub-agent kept", outcome, got)
		}
	}
	if got := endingOf(chat.TimedOut, "", askBy, dialect.Interaction{}, nil); got.Validate() != nil || len(got.Prompt.Questions) != 0 {
		t.Errorf("an unreadable request: ending = %+v, %v; want one that is still valid", got, got.Validate())
	}
}
