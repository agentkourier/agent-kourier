package interact

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
)

func many(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("choice-%d", i)
	}
	return out
}

// interaction has one question of each kind of control courier renders.
func interaction() dialect.Interaction {
	return dialect.Interaction{Kind: dialect.AskUser, RequestID: "r1", Hint: "h", Questions: []dialect.Question{
		{Text: "free"}, // 0 text
		{Text: "one", Choices: []string{"red", "green", "blue"}},         // 1 radio
		{Text: "some", Choices: []string{"a", "b", "c"}, Multiple: true}, // 2 checkboxes
		{Text: "pick", Choices: many(120)},                               // 3 numbers (over 100)
		{Text: "picks", Choices: many(120), Multiple: true},              // 4 numbers, many
	}}
}

func press(t *testing.T, in dialect.Interaction, answers map[int]slack.Input) (dialect.Answer, []Problem) {
	t.Helper()
	controls, err := Controls("ix-1", in)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]map[string]slack.Input{}
	for i, a := range answers {
		inputs[controls[i].BlockID] = map[string]slack.Input{controls[i].ActionID: a}
	}
	answer, problems, err := ExtractAnswer(in, controls, inputs)
	if err != nil {
		t.Fatal(err)
	}
	return answer, problems
}

func text(vals ...string) slack.Input   { return slack.Input{Type: "plain_text_input", Values: vals} }
func radio(vals ...string) slack.Input  { return slack.Input{Type: "radio_buttons", Values: vals} }
func checks(vals ...string) slack.Input { return slack.Input{Type: "checkboxes", Values: vals} }

// The controls of a prompt are pinned: a press is read through them, so a change of layout under a
// pending prompt must be a deliberate one. It fails closed (the answers read as unanswered).
func TestControlsGolden(t *testing.T) {
	controls, err := Controls("ix-1", interaction())
	if err != nil {
		t.Fatal(err)
	}
	want := []render.Control{{BlockID: "q0", ActionID: "a0", Kind: render.ControlText, Element: "plain_text_input"}, {BlockID: "q1", ActionID: "a1", Kind: render.ControlChoice, Element: "radio_buttons"},
		{BlockID: "q2", ActionID: "a2", Kind: render.ControlChoices, Element: "checkboxes"}, {BlockID: "q3", ActionID: "a3", Kind: render.ControlNumbers, Element: "plain_text_input"},
		{BlockID: "q4", ActionID: "a4", Kind: render.ControlNumbers, Element: "plain_text_input"}}
	if !reflect.DeepEqual(controls, want) {
		t.Errorf("controls = %+v; want %+v", controls, want)
	}
}

func TestExtractAnswerMapsIndicesBackToChoices(t *testing.T) {
	in := interaction()
	got, problems := press(t, in, map[int]slack.Input{0: text("  because  "), 1: radio("2"), 2: checks("0", "2", "0"), 3: text("2"), 4: text("1, 3;3\t5")})
	want := [][]string{{"because"}, {"blue"}, {"a", "c"}, {in.Questions[3].Choices[1]}, {in.Questions[4].Choices[0], in.Questions[4].Choices[2], in.Questions[4].Choices[4]}}
	if len(problems) != 0 || got.RequestID != "r1" || !reflect.DeepEqual(got.Answers, want) {
		t.Errorf("answer = %+v, problems %v; want %v", got, problems, want)
	}
}

// Everything that is not an answer is unanswered, whichever way Slack says nothing.
func TestExtractAnswerUnanswered(t *testing.T) {
	for name, a := range map[string]*slack.Input{
		"a missing block": nil,
		"null":            {Type: "plain_text_input"},
		"an empty list":   {Type: "plain_text_input", Values: []string{}},
		"an empty string": {Type: "plain_text_input", Values: []string{""}},
		"white space":     {Type: "plain_text_input", Values: []string{" \t\n"}},
		"a wrong type":    {Type: "number_input", Values: []string{"7"}},
		"another control": {Type: "checkboxes", Values: []string{"0"}},
	} {
		t.Run(name, func(t *testing.T) {
			answers := map[int]slack.Input{1: radio("0"), 2: checks("1"), 3: text("1"), 4: text("2")}
			if a != nil {
				answers[0] = *a
			}
			got, problems := press(t, interaction(), answers)
			if !reflect.DeepEqual(problems, []Problem{{0, Unanswered}}) || got.Answers[0] != nil {
				t.Errorf("answer %v, problems %v; want question 1 unanswered and the rest read", got.Answers, problems)
			}
		})
	}
}

// What no honest press holds is invalid, never read as a different answer.
func TestExtractAnswerInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		q  int
		in slack.Input
	}{
		"a non-canonical index":         {1, radio("01")},
		"a signed index":                {1, radio("+1")},
		"a padded index":                {1, radio(" 1")},
		"an index that is not a number": {1, radio("green")},
		"an index out of range":         {1, radio("3")},
		"a negative index":              {1, radio("-1")},
		"two choices for one":           {1, radio("0", "1")},
		"one bad index among good":      {2, checks("0", "9")},
		"two answers for a text":        {0, text("a", "b")},
		"a number out of range":         {3, text("121")},
		"zero":                          {3, text("0")},
		"a word":                        {3, text("two")},
		"a range":                       {4, text("1-3")},
		"a signed number":               {4, text("+2")},
		"a padded number":               {4, text("02")},
		"two numbers for one":           {3, text("1, 2")},
		"two values in a numbers input": {4, text("1", "2")},
	} {
		t.Run(name, func(t *testing.T) {
			answers := map[int]slack.Input{0: text("x"), 1: radio("0"), 2: checks("1"), 3: text("1"), 4: text("2")}
			answers[tc.q] = tc.in
			if _, problems := press(t, interaction(), answers); !reflect.DeepEqual(problems, []Problem{{tc.q, Invalid}}) {
				t.Errorf("problems = %v; want question %d invalid", problems, tc.q+1)
			}
		})
	}
}

// A layout that does not match the question list reads as unanswered, never as a shifted answer.
func TestExtractAnswerFailsClosedOnAMismatchedLayout(t *testing.T) {
	in := interaction()
	controls, _ := Controls("ix-1", in)
	inputs := map[string]map[string]slack.Input{"q0": {"a0": text("x")}}
	got, problems, err := ExtractAnswer(in, controls[:2], inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != len(in.Questions) || problems[0].Kind != Unanswered || got.Answers[0] != nil {
		t.Errorf("answer %v, problems %v; want every question unanswered", got.Answers, problems)
	}
}

// A value is read only from the element type its question was rendered as: a radio prompt takes
// radio_buttons, not another input that holds a choice, however well formed its value.
func TestExtractAnswerReadsOnlyTheRenderedElementType(t *testing.T) {
	for name, tc := range map[string]struct {
		q  int
		in slack.Input
	}{
		"checkboxes on a radio question":   {1, checks("0")},
		"a select on a radio question":     {1, slack.Input{Type: "static_select", Values: []string{"0"}}},
		"a multi-select on a checkbox":     {2, slack.Input{Type: "multi_static_select", Values: []string{"0"}}},
		"radio buttons on a checkbox":      {2, radio("0")},
		"a checkbox in a numbers input":    {3, checks("1")},
		"a text input on a radio question": {1, text("0")},
		"a radio value on a text question": {0, radio("0")},
	} {
		t.Run(name, func(t *testing.T) {
			answers := map[int]slack.Input{0: text("x"), 1: radio("0"), 2: checks("1"), 3: text("1"), 4: text("2")}
			answers[tc.q] = tc.in
			if _, problems := press(t, interaction(), answers); !reflect.DeepEqual(problems, []Problem{{tc.q, Unanswered}}) {
				t.Errorf("problems = %v; want question %d unanswered", problems, tc.q+1)
			}
		})
	}
}

// More controls than questions is a layout that does not match too: every question reads as unanswered.
func TestExtractAnswerFailsClosedOnExtraControls(t *testing.T) {
	in := interaction()
	controls, _ := Controls("ix-1", in)
	inputs := map[string]map[string]slack.Input{"q0": {"a0": text("x")}, "q1": {"a1": radio("0")}}
	extra := append(slices.Clone(controls), render.Control{BlockID: "q9", ActionID: "a9", Kind: render.ControlText, Element: "plain_text_input"})
	got, problems, err := ExtractAnswer(in, extra, inputs)
	if err != nil || len(problems) != len(in.Questions) || got.Answers[0] != nil || got.Answers[1] != nil {
		t.Errorf("answer %v, problems %v, %v; want every question unanswered", got.Answers, problems, err)
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
		controls, _ := Controls("ix-1", in)
		mutate(&in)
		got, problems, err := ExtractAnswer(in, controls, map[string]map[string]slack.Input{"q0": {"a0": text("x")}})
		if !errors.Is(err, ErrNotAskUser) || len(problems) != 0 || got.Answers != nil {
			t.Errorf("%s: answer %v, problems %v, err %v; want ErrNotAskUser and nothing read", name, got.Answers, problems, err)
		}
	}
}

// TestAControlOfAKindNotKnownReadsAsUnanswered: a kind added to render later must not be read as answered
// before extract learns it.
func TestAControlOfAKindNotKnownReadsAsUnanswered(t *testing.T) {
	q := dialect.Question{Text: "Which?", Choices: []string{"a", "b"}}
	c := render.Control{Kind: render.ControlKind(99), Element: "plain_text_input"}
	if vals, why, ok := extract(q, c, slack.Input{Type: "plain_text_input", Values: []string{"a"}}); ok || why != Unanswered || vals != nil {
		t.Errorf("extract = %v, %v, %v; want it unanswered", vals, why, ok)
	}
}
