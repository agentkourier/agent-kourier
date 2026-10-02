package interact

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

func many(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("choice-%d", i)
	}
	return out
}

// interaction has one question of each shape: free text, one choice, some choices, and two over many choices.
func interaction() dialect.Interaction {
	return dialect.Interaction{Kind: dialect.AskUser, RequestID: "r1", Hint: "h", Questions: []dialect.Question{
		{Text: "free"}, // 0 text
		{Text: "one", Choices: []string{"red", "green", "blue"}},         // 1 one choice
		{Text: "some", Choices: []string{"a", "b", "c"}, Multiple: true}, // 2 some choices
		{Text: "pick", Choices: many(120)},                               // 3 one of many
		{Text: "picks", Choices: many(120), Multiple: true},              // 4 some of many
	}}
}

// answers is what an adapter hands over for the answers of the questions given, in question order.
func answers(byQuestion map[int][]string) []chat.Answer {
	var out []chat.Answer
	for q := range 100 {
		if vals, ok := byQuestion[q]; ok {
			out = append(out, chat.Answer{Question: q, Values: vals})
		}
	}
	return out
}

func press(t *testing.T, in dialect.Interaction, given []chat.Answer) (dialect.Answer, []Problem) {
	t.Helper()
	answer, problems, err := ExtractAnswer(in, given)
	if err != nil {
		t.Fatal(err)
	}
	return answer, problems
}

// valid is an answer to every question of interaction().
func valid() map[int][]string {
	return map[int][]string{0: {"x"}, 1: {"0"}, 2: {"1"}, 3: {"1"}, 4: {"2"}}
}

func TestExtractAnswerMapsIndicesBackToChoices(t *testing.T) {
	in := interaction()
	got, problems := press(t, in, answers(map[int][]string{0: {"  because  "}, 1: {"2"}, 2: {"0", "2", "0"}, 3: {"1"}, 4: {"0", "2", "4"}}))
	want := [][]string{{"because"}, {"blue"}, {"a", "c"}, {in.Questions[3].Choices[1]}, {in.Questions[4].Choices[0], in.Questions[4].Choices[2], in.Questions[4].Choices[4]}}
	if len(problems) != 0 || got.RequestID != "r1" || !reflect.DeepEqual(got.Answers, want) {
		t.Errorf("answer = %+v, problems %v; want %v", got, problems, want)
	}
}

// The last question is read, and the answers need not come in order.
func TestExtractAnswerReadsTheLastQuestionInAnyOrder(t *testing.T) {
	in := interaction()
	given := []chat.Answer{{Question: 4, Values: []string{"119"}}, {Question: 0, Values: []string{"x"}}, {Question: 3, Values: []string{"0"}}, {Question: 2, Values: []string{"1"}}, {Question: 1, Values: []string{"0"}}}
	got, problems := press(t, in, given)
	if len(problems) != 0 || got.Answers[4][0] != "choice-119" {
		t.Errorf("answer %v, problems %v; want every question read, the last by its index", got.Answers, problems)
	}
}

// Everything that is not an answer is unanswered, whichever way the platform says nothing.
func TestExtractAnswerUnanswered(t *testing.T) {
	for name, a := range map[string][]string{
		"no answer":       nil, // the key is left out below
		"null":            nil,
		"an empty list":   {},
		"an empty string": {""},
		"white space":     {" \t\n"},
		"only blanks":     {"", " "},
	} {
		t.Run(name, func(t *testing.T) {
			byQuestion := valid()
			delete(byQuestion, 0)
			if name != "no answer" {
				byQuestion[0] = a
			}
			got, problems := press(t, interaction(), answers(byQuestion))
			if !reflect.DeepEqual(problems, []Problem{{0, Unanswered}}) || got.Answers[0] != nil {
				t.Errorf("answer %v, problems %v; want question 1 unanswered and the rest read", got.Answers, problems)
			}
		})
	}
}

// What no honest press holds is invalid, never read as a different answer.
func TestExtractAnswerInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		q    int
		vals []string
	}{
		"a non-canonical index":         {1, []string{"01"}},
		"a signed index":                {1, []string{"+1"}},
		"a padded index":                {1, []string{" 1"}},
		"an index that is not a number": {1, []string{"green"}},
		"an index out of range":         {1, []string{"3"}},
		"a negative index":              {1, []string{"-1"}},
		"two choices for one":           {1, []string{"0", "1"}},
		"one bad index among good":      {2, []string{"0", "9"}},
		"two answers for a text":        {0, []string{"a", "b"}},
		"an index past many choices":    {3, []string{"120"}},
		"a word":                        {3, []string{"two"}},
		"a range":                       {4, []string{"1-3"}},
		"a signed number":               {4, []string{"+2"}},
		"a padded number":               {4, []string{"02"}},
		"a typed list in one value":     {4, []string{"1, 2"}},
		"two for a single choice":       {3, []string{"1", "2"}},
	} {
		t.Run(name, func(t *testing.T) {
			byQuestion := valid()
			byQuestion[tc.q] = tc.vals
			if _, problems := press(t, interaction(), answers(byQuestion)); !reflect.DeepEqual(problems, []Problem{{tc.q, Invalid}}) {
				t.Errorf("problems = %v; want question %d invalid", problems, tc.q+1)
			}
		})
	}
}

// An answer to a question the prompt does not have names no control of it: the press is refused as invalid,
// whole, and nothing in it is read. The decode of an adapter does not bound the index, so this does.
func TestExtractAnswerRefusesAnAnswerToAQuestionThatIsNotThere(t *testing.T) {
	in := interaction()
	n := len(in.Questions)
	for name, stray := range map[string]int{"the first one past": n, "far past": 9999, "a negative index": -1} {
		t.Run(name, func(t *testing.T) {
			given := append(answers(valid()), chat.Answer{Question: stray, Values: []string{"0"}})
			got, problems := press(t, in, given)
			if len(problems) != n {
				t.Fatalf("problems = %v; want every question refused, since the press is not an honest one", problems)
			}
			for i, p := range problems {
				if p != (Problem{i, Invalid}) {
					t.Errorf("problem %d = %+v; want question %d invalid", i, p, i+1)
				}
			}
			if got.Answers[0] != nil {
				t.Errorf("answers %v; want none of the press read", got.Answers)
			}
		})
	}
}

// A question that is answered twice cannot be told which is meant.
func TestExtractAnswerRefusesAQuestionAnsweredTwice(t *testing.T) {
	given := append(answers(valid()), chat.Answer{Question: 1, Values: []string{"2"}})
	if _, problems := press(t, interaction(), given); !reflect.DeepEqual(problems, []Problem{{1, Invalid}}) {
		t.Errorf("problems = %v; want question 2 invalid", problems)
	}
}

// Only an ask-user pause with questions is read; anything else is an error, and no answer.
func TestExtractAnswerRefusesWhatIsNotAnAskUserPause(t *testing.T) {
	for name, mutate := range map[string]func(*dialect.Interaction){
		"a tool approval": func(in *dialect.Interaction) { in.Kind = dialect.ToolApproval },
		"no kind":         func(in *dialect.Interaction) { in.Kind = "" },
		"no questions":    func(in *dialect.Interaction) { in.Questions = nil },
	} {
		in := interaction()
		mutate(&in)
		got, problems, err := ExtractAnswer(in, answers(valid()))
		if !errors.Is(err, ErrNotAskUser) || len(problems) != 0 || got.Answers != nil {
			t.Errorf("%s: answer %v, problems %v, err %v; want ErrNotAskUser and nothing read", name, got.Answers, problems, err)
		}
	}
}
