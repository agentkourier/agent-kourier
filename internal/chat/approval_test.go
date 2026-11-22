package chat_test

import (
	"encoding"
	"errors"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/redact"
)

func TestAPromptHoldsQuestionsOrToolsNeverBothNorNeither(t *testing.T) {
	questions := []chat.Question{{Text: "q"}}
	tools := []chat.Tool{{Name: "kubectl", Args: `{"verb":"delete"}`}}
	for name, tc := range map[string]struct {
		p  chat.Prompt
		ok bool
	}{
		"questions":                {chat.Prompt{ID: "ix-1", Questions: questions}, true},
		"tools":                    {chat.Prompt{ID: "ix-1", Tools: tools}, true},
		"a tool with no arguments": {chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "ping"}}}, true},
		"a tool with no name":      {chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Args: "{}"}}}, true},
		"both":                     {chat.Prompt{ID: "ix-1", Questions: questions, Tools: tools}, false},
		"neither":                  {chat.Prompt{ID: "ix-1"}, false},
		"tools, no ID":             {chat.Prompt{Tools: tools}, false},
	} {
		err := tc.p.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if !tc.ok && !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAnEndingSaysApprovedOrDeniedForEachToolInOrder(t *testing.T) {
	two := chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "a"}, {Name: "b"}}}
	for name, tc := range map[string]struct {
		e  chat.Ending
		ok bool
	}{
		"answered, one verdict for each":     {chat.Ending{Outcome: chat.Answered, Prompt: two, Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied}}, true},
		"timed out, none":                    {chat.Ending{Outcome: chat.TimedOut, Prompt: two}, true},
		"cancelled, none":                    {chat.Ending{Outcome: chat.Cancelled, Prompt: two}, true},
		"answered questions, none":           {chat.Ending{Outcome: chat.Answered, Prompt: chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}}, Answers: [][]string{{"x"}}}, true},
		"answered tools with no verdicts":    {chat.Ending{Outcome: chat.Answered, Prompt: two}, false},
		"answered, too few verdicts":         {chat.Ending{Outcome: chat.Answered, Prompt: two, Verdicts: []chat.ToolVerdict{chat.ToolApproved}}, false},
		"answered, too many verdicts":        {chat.Ending{Outcome: chat.Answered, Prompt: two, Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied, chat.ToolDenied}}, false},
		"an unknown verdict":                 {chat.Ending{Outcome: chat.Answered, Prompt: two, Verdicts: []chat.ToolVerdict{chat.ToolApproved, "maybe"}}, false},
		"verdicts for a prompt of questions": {chat.Ending{Outcome: chat.Answered, Prompt: chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}}, Verdicts: []chat.ToolVerdict{chat.ToolApproved}}, false},
	} {
		err := tc.e.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if !tc.ok && !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAnOutgoingValidateChecksTheToolFormToo(t *testing.T) {
	both := chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}, Tools: []chat.Tool{{Name: "a"}}}
	if err := chat.Ask(both).Validate(); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Ask(both).Validate = %v, want ErrInvalid", err)
	}
	if err := chat.Ask(chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "a"}}}).Validate(); err != nil {
		t.Errorf("Ask(tools).Validate = %v", err)
	}
	e := chat.Ending{Outcome: chat.Answered, Prompt: chat.Prompt{Tools: []chat.Tool{{Name: "a"}}}}
	if err := chat.End(e).Validate(); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("End(answered, no verdicts).Validate = %v, want ErrInvalid", err)
	}
}

func TestThePressedButtonIsSubmitUnlessSaidOtherwise(t *testing.T) {
	if (chat.Interaction{}).Action != chat.ActionSubmit {
		t.Errorf("the zero Interaction presses %v, want submit: a press recorded before the action existed is a submit", (chat.Interaction{}).Action)
	}
	for a, want := range map[chat.Action]string{chat.ActionSubmit: "submit", chat.ActionApproveAll: "approve_all", chat.ActionDenyAll: "deny_all", chat.Action(99): "unknown"} {
		if a.String() != want {
			t.Errorf("Action(%d).String = %q, want %q", int(a), a.String(), want)
		}
	}
}

func TestAnActionIsStoredByNameAndReadBackOrRefused(t *testing.T) {
	for _, a := range []chat.Action{chat.ActionSubmit, chat.ActionApproveAll, chat.ActionDenyAll} {
		text, err := a.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v) = %v", a, err)
		}
		var back chat.Action
		if err := back.UnmarshalText(text); err != nil || back != a {
			t.Errorf("%v stored as %q and read back as %v, %v", a, text, back, err)
		}
	}
	if _, err := chat.Action(99).MarshalText(); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("MarshalText of an unknown action = %v, want ErrInvalid", err)
	}
	var a chat.Action
	if err := a.UnmarshalText([]byte("approve-everything")); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("UnmarshalText of an unknown name = %v, want ErrInvalid", err)
	}
	var _ encoding.TextMarshaler = chat.ActionSubmit
	var _ encoding.TextUnmarshaler = &a
}

func TestAToolAnswerIsAFixedPairOfValues(t *testing.T) {
	if chat.ToolApproveValue != "0" || chat.ToolDenyValue != "1" {
		t.Errorf("approve %q, deny %q: stored presses carry these, so they are fixed at %q and %q", chat.ToolApproveValue, chat.ToolDenyValue, "0", "1")
	}
	for name, tc := range map[string]struct {
		a    chat.Answer
		want chat.ToolVerdict
		ok   bool
	}{
		"approve":          {chat.Answer{Question: 2, Values: []string{"0"}}, chat.ToolApproved, true},
		"deny":             {chat.Answer{Question: 2, Values: []string{"1"}}, chat.ToolDenied, true},
		"no values":        {chat.Answer{Question: 2}, "", false},
		"two values":       {chat.Answer{Question: 2, Values: []string{"0", "1"}}, "", false},
		"an unknown index": {chat.Answer{Question: 2, Values: []string{"2"}}, "", false},
		"text":             {chat.Answer{Question: 2, Values: []string{"approve"}}, "", false},
		"a padded index":   {chat.Answer{Question: 2, Values: []string{"00"}}, "", false},
	} {
		got, ok := tc.a.ToolVerdict()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: ToolVerdict = %q, %t; want %q, %t", name, got, ok, tc.want, tc.ok)
		}
	}
}

// The ending of a prompt of tools shows each tool's arguments on one line, as a status line shows a call's: a JSON object as
// "key=value, ..." in the order of its keys, any other text as it is, a secret that only the line shows redacted, and a long
// summary cut and marked.
func TestAToolsSummaryIsItsArgumentsOnOneLine(t *testing.T) {
	long := `{"command":"` + strings.Repeat("x", 300) + `"}`
	for name, tc := range map[string]struct{ args, want string }{
		"none":                    {"", ""},
		"blank":                   {" \n ", ""},
		"an empty object":         {"{}", ""},
		"one argument":            {"{\n  \"command\": \"kubectl get pods -n shop -o wide\"\n}", `command="kubectl get pods -n shop -o wide"`},
		"keys in order":           {`{"ns":"prod","count":3,"force":true,"labels":{"app":"web"},"tags":["a","b"],"none":null}`, `count=3, force=true, labels={"app":"web"}, none=null, ns="prod", tags=["a","b"]`},
		"a number as written":     {`{"n":1.50,"big":12345678901234567890}`, `big=12345678901234567890, n=1.50`},
		"markup kept as text":     {`{"q":"<!channel> & <b>"}`, `q="<!channel> & <b>"`},
		"a line break in a value": {`{"script":"set -e\nmake"}`, `script="set -e\nmake"`},
		"text that is no object":  {"n=3\nforce", "n=3 force"},
		"a JSON array":            {`["a"]`, `["a"]`},
		"an object and more":      {`{"a":1} x`, `{"a":1} x`},
		"a redacted value":        {`{"password":"` + redact.Redacted + `","user":"ops"}`, `password="` + redact.Redacted + `", user="ops"`},
		"long":                    {long, `command="` + strings.Repeat("x", chat.MaxToolSummary-len(`command="`)-chat.Units(chat.Truncated)) + chat.Truncated},
		"a long word":             {`{"blob":"` + strings.Repeat("中", 5000) + `"}`, `blob="` + strings.Repeat("中", chat.MaxToolSummary-len(`blob="`)-chat.Units(chat.Truncated)) + chat.Truncated},
	} {
		got := chat.Tool{Name: "t", Args: tc.args}.Summary()
		if got != tc.want {
			t.Errorf("%s: Summary() = %q, want %q", name, got, tc.want)
		}
		if strings.ContainsAny(got, "\n\r") || chat.Units(got) > chat.MaxToolSummary {
			t.Errorf("%s: %q is not one line of at most %d units", name, got, chat.MaxToolSummary)
		}
	}
}
