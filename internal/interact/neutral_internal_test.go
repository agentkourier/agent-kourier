package interact

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
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

func TestPromptOfRefusesAPauseOfAnotherKind(t *testing.T) {
	other := dialect.Interaction{Kind: "something_else", TaskID: "t1", RequestID: "r1", Questions: []dialect.Question{{Text: "q"}}}
	if _, err := promptOf("ix-1", other, askBy); err == nil {
		t.Error("a pause of an unknown kind was made into a prompt")
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
	got, err := endingOf(chat.Answered, "UALICE1", askBy, in, answers)
	if err != nil {
		t.Fatal(err)
	}
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
		got, err := endingOf(outcome, "", askBy, in, [][]string{{"a"}})
		if err != nil || got.Answers != nil || got.Outcome != outcome || got.Prompt.Subagent != "planner" {
			t.Errorf("%s: ending = %+v; want no answers, and the sub-agent kept", outcome, got)
		}
	}
	if got, err := endingOf(chat.TimedOut, "", askBy, dialect.Interaction{}, nil); err != nil || got.Validate() != nil || len(got.Prompt.Questions) != 0 {
		t.Errorf("an unreadable request: ending = %+v, %v, %v; want one that is still valid", got, err, got.Validate())
	}
}

// secretPause is a tool approval whose arguments hold a secret under a secret name, in a header, and in a
// nested list, and whose hint and tool name hold one too.
func secretPause() dialect.Interaction {
	return dialect.Interaction{
		Kind: dialect.ToolApproval, TaskID: "t1", Hint: "run it, token=" + plantedInHint, Nested: &dialect.Nested{Subagent: "planner token=" + plantedInSubagent},
		Tools: []dialect.Tool{
			{ID: "c1", Name: "run_command", Args: map[string]any{"password": plantedInArgs, "count": 3, "env": []any{map[string]any{"name": "DB_PASSWORD", "value": plantedInArgs}}}},
			{ID: "c2", Name: "no_args"},
			{ID: "c3", Name: "empty_args", Args: map[string]any{}},
			{ID: "c4", Args: map[string]any{"path": "/x"}}, // a tool with no name
		},
	}
}

const (
	plantedInArgs     = "hunter2-planted-in-the-arguments"
	plantedInHint     = "planted-token-in-the-hint"
	plantedInSubagent = "planted-token-in-the-subagent"
)

// The prompt of an approval has a tool for each tool of the pause, in the pause's order, and shows what is
// stored of it: the arguments, the tool names, the hint and the sub-agent with the secrets taken out, as indented
// JSON with sorted keys, so the same pause always renders the same text.
func TestPromptOfAnApprovalShowsWhatIsStoredOfIt(t *testing.T) {
	in := secretPause()
	got, err := promptOf("ix-1", in, askBy)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.Prompt{ID: "ix-1", Deadline: askBy, Subagent: "planner token=" + redactedWord, Hint: "run it, token=" + redactedWord, Tools: []chat.Tool{
		{Name: "run_command", Args: "{\n  \"count\": 3,\n  \"env\": [\n    {\n      \"name\": \"DB_PASSWORD\",\n      \"value\": \"" + redactedWord + "\"\n    }\n  ],\n  \"password\": \"" + redactedWord + "\"\n}"},
		{Name: "no_args"},
		{Name: "empty_args"},
		{Name: "", Args: "{\n  \"path\": \"/x\"\n}"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("promptOf = %+v, want %+v", got, want)
	}
	if got.Validate() != nil {
		t.Errorf("the prompt is not valid: %v", got.Validate())
	}
	for range 20 { // maps have no order in Go; the text must not either
		again, err := promptOf("ix-1", in, askBy)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("the same pause made another prompt: %+v, %v", again, err)
		}
	}
	if in.Tools[0].Args["password"] != plantedInArgs || !strings.Contains(in.Hint, plantedInHint) || !strings.Contains(in.Nested.Subagent, plantedInSubagent) {
		t.Error("promptOf changed the pause it was given")
	}
}

// redactedWord is what the redaction puts in place of a secret.
const redactedWord = "[redacted]"

// The arguments are text a person reads to decide: markup characters are shown as they are, not as JSON's
// \u003c escapes.
func TestPromptOfAnApprovalDoesNotEscapeMarkupInArguments(t *testing.T) {
	in := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "t1", Tools: []dialect.Tool{{ID: "c1", Name: "sh", Args: map[string]any{"cmd": "a < b && c > d"}}}}
	got, err := promptOf("ix-1", in, askBy)
	if err != nil || got.Tools[0].Args != "{\n  \"cmd\": \"a < b && c > d\"\n}" {
		t.Errorf("args = %q, %v; want the characters as they are", got.Tools[0].Args, err)
	}
}

// What the sweep and the replacement build an ending from is the stored request, which already has its secrets
// out. Its prompt says what the one the person saw said: redacting twice changes nothing.
func TestAnApprovalBuiltFromTheStoredRequestIsTheSameAsFromThePause(t *testing.T) {
	in := secretPause()
	shown, err := promptOf("ix-1", in, askBy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := promptOf("ix-1", storable(in), askBy)
	if err != nil || !reflect.DeepEqual(got, shown) {
		t.Errorf("from the stored request: %+v, %v; want %+v", got, err, shown)
	}
}

// The ending of an approval that timed out or was cancelled shows its tools, as stored, with no verdicts, and is
// valid. One that was answered is not built here: its verdicts are not.
func TestEndingOfAnApprovalNeedsNoVerdictsWhenItWasNotAnswered(t *testing.T) {
	in := storable(secretPause())
	for _, outcome := range []chat.Outcome{chat.TimedOut, chat.Cancelled} {
		got, err := endingOf(outcome, "", askBy, in, nil)
		if err != nil || got.Validate() != nil {
			t.Fatalf("%s: ending = %+v, %v, %v; want a valid one", outcome, got, err, got.Validate())
		}
		shown, _ := promptOf("", in, time.Time{})
		if !reflect.DeepEqual(got.Prompt, shown) || got.Verdicts != nil || got.Answers != nil || got.Prompt.Subagent != "planner token="+redactedWord || len(got.Prompt.Tools) != 4 {
			t.Errorf("%s: ending prompt = %+v; want the tools as stored, the sub-agent, and no verdicts", outcome, got.Prompt)
		}
	}
	if _, err := endingOf(chat.Answered, "UALICE1", askBy, in, nil); err == nil {
		t.Error("an answered approval was given an ending without verdicts")
	}
}

// A hint and a sub-agent's name are context and not what runs, so the prompt may show them cut (the redaction bounds
// each to 4 KiB). What the ending shows of them is what the prompt showed, once cut and not cut again: the ending is
// built from the stored request, which is the cut text.
func TestTheEndingOfAnApprovalShowsWhatThePromptShowed(t *testing.T) {
	in := secretPause()
	in.Hint = strings.Repeat("word ", 1000) // 5000 bytes: cut
	in.Nested.Subagent = "planner " + strings.Repeat("word ", 1000)
	prompt, err := promptOf("ix-1", in, askBy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(prompt.Hint, "(truncated)") || strings.Count(prompt.Hint, "(truncated)") != 1 {
		t.Fatalf("hint %q; want it cut once", prompt.Hint[len(prompt.Hint)-40:])
	}
	for _, outcome := range []chat.Outcome{chat.TimedOut, chat.Cancelled} {
		end, err := endingOf(outcome, "", askBy, storable(in), nil)
		want := prompt
		want.ID, want.Deadline = "", time.Time{}
		if err != nil || !reflect.DeepEqual(end.Prompt, want) {
			t.Errorf("%s: ending prompt = %+v, %v; want what the prompt showed: %+v", outcome, end.Prompt, err, want)
		}
	}
}

// NaN and the infinities are no number a person can read, and JSON cannot write them; if a dialect ever hands one over,
// it is refused like any number that cannot be shown exactly.
func TestExactToolsRefusesANumberThatIsNotExact(t *testing.T) {
	for name, tc := range map[string]struct {
		n  any
		ok bool
	}{
		"NaN": {math.NaN(), false}, "+Inf": {math.Inf(1), false}, "-Inf": {math.Inf(-1), false},
		"2^53": {float64(1 << 53), false}, "-2^53": {-float64(1 << 53), false}, "float32 2^53": {float32(1 << 53), false},
		"2^53-1": {float64(1<<53 - 1), true}, "-(2^53-1)": {-float64(1<<53 - 1), true}, "fraction": {0.1, true}, "tiny": {1.2e-7, true},
		"1e300": {1e300, false}, "an int": {123, true}, "an int64": {int64(1) << 62, true}, "a json.Number": {json.Number("12345678901234567890"), true},
	} {
		err := exactTools([]dialect.Tool{{ID: "c", Name: "n", Args: map[string]any{"v": tc.n}}})
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v; want ok = %v", name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, dialect.ErrUnusablePause) {
			t.Errorf("%s: %v; want an unusable pause", name, err)
		}
	}
}

// The walker names the kinds of value it checks; any other is refused, so that a value it was never taught to check
// (a typed slice or map, which the redaction would not reach into either) is not shown unchecked.
func TestExactToolsRefusesAKindOfValueItDoesNotKnow(t *testing.T) {
	for name, tc := range map[string]struct {
		v  any
		ok bool
	}{
		"string": {"x", true}, "bool": {true, true}, "nil": {nil, true}, "list": {[]any{"x", 1.0, nil}, true}, "map": {map[string]any{"k": "v"}, true},
		"[]string": {[]string{"x"}, false}, "map[string]string": {map[string]string{"password": "x"}, false}, "[]float64": {[]float64{1 << 60}, false},
		"a struct": {struct{ A int }{1}, false}, "a pointer": {new(int), false}, "a typed nil map": {map[string]string(nil), false},
		"a refused kind in a list": {[]any{[]string{"x"}}, false}, "a refused kind in a map": {map[string]any{"k": []string{"x"}}, false},
	} {
		err := exactTools([]dialect.Tool{{ID: "c", Name: "n", Args: map[string]any{"v": tc.v}}})
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, dialect.ErrUnusablePause)) {
			t.Errorf("%s: %v; want ok = %v, and an unusable pause if not", name, err, tc.ok)
		}
	}
}

// The refusals say what is wrong in words of their own and hold nothing of the pause.
func TestExactToolsErrorsHoldNoAgentText(t *testing.T) {
	const marker = "AGENT-TEXT"
	long := strings.Repeat(marker+" ", 1000)
	for name, tool := range map[string]dialect.Tool{
		"a long string":   {ID: marker, Name: marker, Args: map[string]any{marker: long}},
		"a long name":     {ID: marker, Name: long},
		"a long key":      {ID: marker, Name: marker, Args: map[string]any{long: 1}},
		"a big number":    {ID: marker, Name: marker, Args: map[string]any{marker: 1e300}},
		"a control":       {ID: marker, Name: marker, Args: map[string]any{marker: marker + "\x00"}},
		"a key changed":   {ID: marker, Name: marker, Args: map[string]any{marker + "\u200b": 2}},
		"two keys":        {ID: marker, Name: marker, Args: map[string]any{marker: 1, marker + "\u200b": 2}},
		"an unknown kind": {ID: marker, Name: marker, Args: map[string]any{marker: []string{marker}}},
	} {
		err := exactTools([]dialect.Tool{tool})
		if !errors.Is(err, dialect.ErrUnusablePause) || strings.Contains(err.Error(), marker) || len(err.Error()) > 120-len("unusable_pause: ") {
			t.Errorf("%s: %v; want an unusable pause with none of the agent's text, short enough for the audit row to keep whole", name, err)
		}
	}
}

// promptOf checks the approval itself, as Paused does: no caller can make a prompt that shows a tool cut or changed. A name,
// a key or a string that the redaction would cut at 4 KiB is an error that wraps dialect.ErrUnusablePause, and no prompt is
// made. Without its own call to exactTools, promptOf would build a prompt of the stored (cut) text and the person would be
// asked to approve something other than what will run.
func TestPromptOfRefusesAToolTheRedactionWouldCut(t *testing.T) {
	long := strings.Repeat("word ", 1000) // 5000 bytes, past the 4 KiB bound
	for name, tool := range map[string]dialect.Tool{
		"a name":       {ID: "t1", Name: long},
		"a string":     {ID: "t1", Name: "run", Args: map[string]any{"command": long}},
		"a nested one": {ID: "t1", Name: "run", Args: map[string]any{"a": []any{map[string]any{"b": long}}}},
		"a key":        {ID: "t1", Name: "run", Args: map[string]any{strings.Repeat("k", 5000): 1}},
	} {
		in := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "t", Tools: []dialect.Tool{{ID: "t0", Name: "fine"}, tool}}
		p, err := promptOf("ix-1", in, askBy)
		if !errors.Is(err, dialect.ErrUnusablePause) || len(p.Tools) != 0 {
			t.Errorf("%s: promptOf = %+v, %v; want an unusable pause and no prompt", name, p, err)
		}
	}
}

// Text that is not valid UTF-8 is refused wherever a tool call holds text: the name, a key and a string at any depth. The
// cleaning would delete it silently, and a person would read a call other than the one that runs.
func TestExactToolsRefusesInvalidUTF8(t *testing.T) {
	bad := "ab\xffcd"
	for name, tool := range map[string]dialect.Tool{
		"a name":                       {ID: "c", Name: bad},
		"a string":                     {ID: "c", Name: "n", Args: map[string]any{"v": bad}},
		"a key":                        {ID: "c", Name: "n", Args: map[string]any{bad: 1}},
		"a string in a list":           {ID: "c", Name: "n", Args: map[string]any{"v": []any{"ok", bad}}},
		"a string in a nested map":     {ID: "c", Name: "n", Args: map[string]any{"v": map[string]any{"w": bad}}},
		"a truncated multi-byte start": {ID: "c", Name: "n", Args: map[string]any{"v": "x\xe2\x80"}},
		"a lone continuation byte":     {ID: "c", Name: "n", Args: map[string]any{"v": "\x80"}},
	} {
		if err := exactTools([]dialect.Tool{tool}); !errors.Is(err, errToolControl) {
			t.Errorf("%s: %v; want errToolControl", name, err)
		}
	}
	if err := exactTools([]dialect.Tool{{ID: "c", Name: "名前", Args: map[string]any{"キー": "値 é \U0001f600"}}}); err != nil {
		t.Errorf("valid multi-byte text: %v; want it shown", err)
	}
}
