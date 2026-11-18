package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// cardOf is the one Adaptive Card of a, as JSON decodes it, and fails the test if a holds none or more than one.
func cardOf(t *testing.T, a outActivity) map[string]any {
	t.Helper()
	if len(a.Attachments) != 1 || a.Attachments[0].ContentType != cardContentType {
		t.Fatalf("attachments %+v, want one Adaptive Card", a.Attachments)
	}
	raw, err := json.Marshal(a.Attachments[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if card["type"] != "AdaptiveCard" || card["version"] != "1.5" {
		t.Fatalf("card %v, want an AdaptiveCard of version 1.5", card)
	}
	return card
}

// outline is the card's body and actions as one line each: a RichTextBlock as "text:" and its runs joined, with "*" before a bold one,
// "~" before a subtle one and "`" before a monospace one; a TextBlock as "block:" and its text; an input as its type, ID and what it
// offers; an action as "action:" with its title, style and data.
func outline(t *testing.T, card map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := card["body"].([]any)
	for _, it := range items {
		el := it.(map[string]any)
		switch el["type"] {
		case "RichTextBlock":
			var runs []string
			for _, r := range el["inlines"].([]any) {
				run := r.(map[string]any)
				mark := ""
				if run["weight"] == "Bolder" {
					mark += "*"
				}
				if run["isSubtle"] == true {
					mark += "~"
				}
				if run["fontType"] == "Monospace" {
					mark += "`"
				}
				runs = append(runs, mark+run["text"].(string))
			}
			out = append(out, "text:"+strings.Join(runs, "|"))
		case "TextBlock":
			out = append(out, "block:"+el["text"].(string))
		case "Input.Text":
			out = append(out, fmt.Sprintf("Input.Text %s multiline=%v", el["id"], el["isMultiline"]))
		case "Input.ChoiceSet":
			var choices []string
			for _, c := range el["choices"].([]any) {
				ch := c.(map[string]any)
				choices = append(choices, fmt.Sprintf("%s=%s", ch["value"], ch["title"]))
			}
			out = append(out, fmt.Sprintf("Input.ChoiceSet %s %s multi=%v [%s]", el["id"], el["style"], el["isMultiSelect"] == true, strings.Join(choices, " ")))
		default:
			t.Fatalf("an element of the type %v", el["type"])
		}
	}
	actions, _ := card["actions"].([]any)
	for _, a := range actions {
		act := a.(map[string]any)
		if act["type"] != "Action.Execute" {
			t.Fatalf("an action of the type %v", act["type"])
		}
		if act["verb"] != "kourier" {
			t.Errorf("the action %v has the verb %v, want the one fixed word kourier", act["title"], act["verb"])
		}
		data, _ := json.Marshal(act["data"])
		out = append(out, fmt.Sprintf("action:%s %v %s", act["title"], act["style"], data))
	}
	return out
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A prompt of questions is an Adaptive Card and no text: its hint, the sub-agent that asked, each question with its control (a text
// input, radio buttons, or for more than ten choices a menu; checkboxes or a menu that takes several for a question of several
// answers), when it closes, and a Submit button that names the prompt. Every text the agent wrote is a TextRun or a choice's title.
func TestAPromptIsACardWithItsControls(t *testing.T) {
	t.Parallel()
	many := make([]string, inlineChoices+1)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	p := chat.Prompt{ID: "ix-1", Hint: "Deploy", Subagent: "planner", Deadline: time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC), Questions: []chat.Question{
		{Text: "Why?"},
		{Text: "Where?", Choices: []string{"prod", "staging"}},
		{Text: "Which?", Choices: many, Multiple: true},
		{Text: "One of many?", Choices: many},
		{Text: "Some of two?", Choices: []string{"a", " "}, Multiple: true},
		{Text: "One of the most inline?", Choices: many[:inlineChoices]},
	}}
	a := mustRender(t, chat.Ask(p))
	if a.Text != "" || a.TextFormat != "" || len(a.Entities) != 0 {
		t.Errorf("the activity has text %q of the format %q and %d entities, want only the card", a.Text, a.TextFormat, len(a.Entities))
	}
	card := cardOf(t, a)
	if card["fallbackText"] != "The agent needs your input." {
		t.Errorf("fallbackText %q", card["fallbackText"])
	}
	all := "0=c0 1=c1 2=c2 3=c3 4=c4 5=c5 6=c6 7=c7 8=c8 9=c9 10=c10"
	sameLines(t, "the prompt", outline(t, card), []string{
		"text:*Deploy",
		"text:~Asked by the sub-agent planner",
		"text:*Why?",
		"Input.Text t0 multiline=true",
		"text:*Where?",
		"Input.ChoiceSet r1 expanded multi=false [0=prod 1=staging]",
		"text:*Which?",
		"Input.ChoiceSet c2 compact multi=true [" + all + "]",
		"text:*One of many?",
		"Input.ChoiceSet r3 compact multi=false [" + all + "]",
		"text:*Some of two?",
		"Input.ChoiceSet c4 expanded multi=true [0=a 1=(blank)]",
		"text:*One of the most inline?",
		"Input.ChoiceSet r5 expanded multi=false [" + strings.TrimSuffix(all, " 10=c10") + "]",
		"text:~It closes at Wed, 18 Nov 2026 09:30:00 UTC.",
		`action:Submit positive {"kourier":{"action":"submit","prompt":"ix-1"}}`,
	})

	plain := cardOf(t, mustRender(t, chat.Ask(chat.Prompt{ID: "ix-2", Questions: []chat.Question{{Text: " "}}})))
	sameLines(t, "a prompt with no hint", outline(t, plain), []string{
		"text:*The agent needs your input",
		"text:*Question 1",
		"Input.Text t0 multiline=true",
		`action:Submit positive {"kourier":{"action":"submit","prompt":"ix-2"}}`,
	})
}

// A prompt of one tool shows its name and its arguments, and Approve and Deny, which decide it; a prompt of several numbers each
// tool's name, and puts each tool's choice of Approve or Deny just under its arguments, so that no choice reads as another tool's,
// with Submit, Approve all and Deny all under them. A tool with no name is "Tool N", and one with no arguments says so.
func TestAToolApprovalIsACard(t *testing.T) {
	t.Parallel()
	one := cardOf(t, mustRender(t, chat.Ask(chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "scale", Args: "{\n  \"n\": 3\n}"}}})))
	if one["fallbackText"] != "The agent needs your approval." {
		t.Errorf("fallbackText %q", one["fallbackText"])
	}
	sameLines(t, "one tool", outline(t, one), []string{
		"text:*The agent needs your approval",
		"text:*scale",
		"text:`{\n  \"n\": 3\n}",
		`action:Approve positive {"kourier":{"action":"approve_all","prompt":"ix-1"}}`,
		`action:Deny destructive {"kourier":{"action":"deny_all","prompt":"ix-1"}}`,
	})
	several := cardOf(t, mustRender(t, chat.Ask(chat.Prompt{ID: "ix-2", Hint: "Fix it", Tools: []chat.Tool{{Name: "scale", Args: "n=3"}, {Name: " "}}})))
	sameLines(t, "several tools", outline(t, several), []string{
		"text:*Fix it",
		"text:*1. scale",
		"text:`n=3",
		"Input.ChoiceSet r0 expanded multi=false [0=Approve 1=Deny]",
		"text:*Tool 2",
		"text:~(no arguments)",
		"Input.ChoiceSet r1 expanded multi=false [0=Approve 1=Deny]",
		`action:Submit positive {"kourier":{"action":"submit","prompt":"ix-2"}}`,
		`action:Approve all <nil> {"kourier":{"action":"approve_all","prompt":"ix-2"}}`,
		`action:Deny all destructive {"kourier":{"action":"deny_all","prompt":"ix-2"}}`,
	})
}

// A tool is never shown cut: one whose cleaned name or arguments are longer than their place's limit is ErrTooLarge, the number of a
// tool of several counted in its name, and so are tools that are each within it and together more than a message holds.
func TestAToolThatDoesNotFitIsRefused(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	name, args := capabilities().Limit(chat.WhereToolName), capabilities().Limit(chat.WhereToolArgs)
	for what, p := range map[string]chat.Prompt{
		"a long name":     {ID: "ix-1", Tools: []chat.Tool{{Name: strings.Repeat("n", name+1)}}},
		"long arguments":  {ID: "ix-1", Tools: []chat.Tool{{Name: "t", Args: strings.Repeat("a", args+1)}}},
		"twenty together": {ID: "ix-1", Tools: twentyTools(strings.Repeat("中", args))},
		"a numbered name": {ID: "ix-1", Tools: []chat.Tool{{Name: strings.Repeat("n", name-1)}, {Name: "t"}}},
	} {
		if err := c.Fits(p); !errors.Is(err, chat.ErrTooLarge) {
			t.Errorf("%s: Fits gave %v, want ErrTooLarge", what, err)
		}
	}
	if err := c.Fits(chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: strings.Repeat("n", name), Args: strings.Repeat("a", args)}}}); err != nil {
		t.Errorf("a tool at both limits: %v", err)
	}
}

func twentyTools(args string) []chat.Tool {
	tools := make([]chat.Tool, 20)
	for i := range tools {
		tools[i] = chat.Tool{Name: fmt.Sprintf("tool%d", i), Args: args}
	}
	return tools
}

// An ending is a card with no controls: who decided, with a mention of a Microsoft Entra object ID beside fixed words, and what was
// asked and answered, or each tool with what was decided of it and its arguments. A decider of any other shape is "someone", as text.
func TestAnEndingIsACardWithNoControls(t *testing.T) {
	t.Parallel()
	q := chat.Prompt{ID: "ix-1", Hint: "Deploy", Subagent: "planner", Questions: []chat.Question{{Text: "Where?", Choices: []string{"prod", "staging"}}, {Text: "Why?"}}}
	answered := mustRender(t, chat.End(chat.Ending{Outcome: chat.Answered, By: testUser, Prompt: q, Answers: [][]string{{"prod"}, {"(answered)"}}}))
	card := cardOf(t, answered)
	sameLines(t, "answered", outline(t, card), []string{
		"block:<at>someone</at> answered.",
		"text:~Deploy",
		"text:~Asked by the sub-agent planner",
		"text:*Where?",
		"text:Answer: prod",
		"text:*Why?",
		"text:Answer: (answered)",
	})
	entities := card["msteams"].(map[string]any)["entities"].([]any)
	e := entities[0].(map[string]any)
	if len(entities) != 1 || e["type"] != "mention" || e["text"] != "<at>someone</at>" || e["mentioned"].(map[string]any)["id"] != testUser {
		t.Errorf("entities %v", entities)
	}
	if answered.Text != "" || len(answered.Entities) != 0 {
		t.Errorf("the activity has text %q and %d entities", answered.Text, len(answered.Entities))
	}

	tools := chat.Prompt{ID: "ix-2", Tools: []chat.Tool{{Name: "scale", Args: "n=3"}, {Name: "drain"}}}
	for name, tc := range map[string]struct {
		e    chat.Ending
		want []string
	}{
		"tools decided": {chat.Ending{Outcome: chat.Answered, By: "U0FORGED", Prompt: tools, Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied}},
			[]string{"text:someone answered.", "text:*1. scale", "text:Approved", "text:`n=3", "text:*2. drain", "text:Denied", "text:~(no arguments)"}},
		"timed out": {chat.Ending{Outcome: chat.TimedOut, Prompt: tools},
			[]string{"text:Nobody answered in time.", "text:*1. scale", "text:`n=3", "text:*2. drain", "text:~(no arguments)"}},
		"cancelled by someone": {chat.Ending{Outcome: chat.Cancelled, By: testUser, Prompt: chat.Prompt{ID: "ix-3", Questions: []chat.Question{{Text: "Why?"}}}},
			[]string{"block:<at>someone</at> cancelled the question.", "text:*Why?"}},
		"cancelled": {chat.Ending{Outcome: chat.Cancelled, Prompt: chat.Prompt{ID: "ix-3", Questions: []chat.Question{{Text: "Why?"}}}},
			[]string{"text:The question was cancelled.", "text:*Why?"}},
	} {
		c := cardOf(t, mustRender(t, chat.End(tc.e)))
		sameLines(t, name, outline(t, c), tc.want)
		if _, ok := c["actions"]; ok {
			t.Errorf("%s: the ending has actions", name)
		}
	}
}

// An ending cannot be refused, so the longest one fits the budget: twenty tools, each of the longest name and arguments of the
// characters that cost the most bytes, and twenty questions of the longest text with the longest answers, are cut to fit, and the
// cut is marked.
func TestTheLongestEndingFits(t *testing.T) {
	t.Parallel()
	caps := capabilities()
	tools := twentyTools(strings.Repeat("中", caps.Limit(chat.WhereToolArgs)))
	verdicts := make([]chat.ToolVerdict, len(tools))
	for i := range tools {
		tools[i].Name = strings.Repeat("中", caps.Limit(chat.WhereToolName))
		verdicts[i] = chat.ToolApproved
	}
	qs := make([]chat.Question, 20)
	answers := make([][]string, len(qs))
	for i := range qs {
		qs[i] = chat.Question{Text: strings.Repeat("中", caps.Limit(chat.WhereQuestion))}
		answers[i] = []string{strings.Repeat("中", caps.Limit(chat.WhereAnswer))}
	}
	long := strings.Repeat("中", maxTextUnits)
	for name, e := range map[string]chat.Ending{
		"tools":     {Outcome: chat.Answered, By: testUser, Prompt: chat.Prompt{ID: "ix-1", Hint: long, Subagent: long, Tools: tools}, Verdicts: verdicts},
		"questions": {Outcome: chat.Answered, By: testUser, Prompt: chat.Prompt{ID: "ix-1", Hint: long, Subagent: long, Questions: qs}, Answers: answers},
	} {
		m, err := render(chat.End(e))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a := m.activity
		n, err := size(a)
		if err != nil || n > maxMessageBytes {
			t.Errorf("%s: %d bytes, %v; want at most %d", name, n, err, maxMessageBytes)
		}
		raw, _ := json.Marshal(a.Attachments[0].Content)
		if !strings.Contains(string(raw), chat.Truncated) {
			t.Errorf("%s: the cut is not marked", name)
		}
	}
}

// A message's buttons are the actions of a card of their own, posted after its text, each named by its ID and value; the text holds
// the rest of the message, and no label. A message of buttons alone is the card, with no text; with a fallback, the fallback is its
// text, and the buttons go after it.
func TestButtonsAreTheActionsOfACardOfTheirOwn(t *testing.T) {
	t.Parallel()
	m, err := render(chat.Say(chat.Message{Parts: []chat.Part{
		chat.Body{Markdown: "The pods cannot read their logs."},
		chat.Context{Text: "Was this useful?"},
		chat.Buttons{Buttons: []chat.Button{{ID: "useful", Label: "Useful", Value: "fb1|a", Primary: true}, {ID: "not", Label: "Not useful", Value: "fb1|b"}}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if a := m.activity; a.Text != "The pods cannot read their logs.\n\nWas this useful?" || a.TextFormat != "markdown" || len(a.Attachments) != 0 {
		t.Errorf("text %q of the format %q, %d attachments", a.Text, a.TextFormat, len(a.Attachments))
	}
	card := cardOf(t, buttonsActivity(m.buttons, "1700000000001"))
	if _, ok := card["body"]; ok {
		t.Errorf("the card of buttons has a body: %v", card["body"])
	}
	sameLines(t, "buttons", outline(t, card), []string{
		`action:Useful positive {"kourier":{"button":"useful","of":"1700000000001","value":"fb1|a"}}`,
		`action:Not useful <nil> {"kourier":{"button":"not","of":"1700000000001","value":"fb1|b"}}`,
	})
	alone, err := render(chat.Say(chat.Message{Parts: []chat.Part{chat.Buttons{Buttons: []chat.Button{{ID: "b", Label: "B", Value: "v"}}}}}))
	if err != nil || alone.activity.Text != "" || len(alone.buttons) != 0 {
		t.Fatalf("buttons alone: %+v, %v", alone, err)
	}
	sameLines(t, "buttons alone", outline(t, cardOf(t, alone.activity)), []string{`action:B <nil> {"kourier":{"button":"b","value":"v"}}`})
	only, err := render(chat.Say(chat.Message{Fallback: "Rate it", Parts: []chat.Part{chat.Buttons{Buttons: []chat.Button{{ID: "b", Label: "B", Value: "v"}}}}}))
	if err != nil || only.activity.Text != "Rate it" || len(only.activity.Attachments) != 0 || len(only.buttons) != 1 {
		t.Errorf("buttons with a fallback: %+v, %v", only, err)
	}
	if none := mustRender(t, chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{chat.Buttons{}}})); len(none.Attachments) != 0 {
		t.Errorf("a Buttons part with no buttons made a card: %+v", none.Attachments)
	}
}

// The choices that a question shows inline, and the shortest that an ending's texts are cut to, are held to the limits page.
func TestCardBoundsAreOnTheLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "teamsInlineChoices", inlineChoices)
	limitsdoc.Count(t, "teamsMinEndingUnits", minEndingUnits)
}

// An ending over the budget is cut a half of the cap at a time, and no further than it must: one that fits with its texts at half the
// cap is cut to that, and not to the floor.
func TestAnEndingIsCutNoFurtherThanItMust(t *testing.T) {
	t.Parallel()
	half := maxTextUnits / 2
	qs := make([]chat.Question, 20)
	answers := make([][]string, len(qs))
	for i := range qs {
		qs[i] = chat.Question{Text: strings.Repeat("a", maxTextUnits)}
		answers[i] = []string{strings.Repeat("b", maxTextUnits)}
	}
	e := chat.Ending{Outcome: chat.Answered, By: testUser, Prompt: chat.Prompt{ID: "ix-1", Questions: qs}, Answers: answers}
	uncut := cardBuilder{caps: capabilities()}
	if n, err := size(cardActivity(uncut.ending(e))); err != nil || n <= maxMessageBytes {
		t.Fatalf("the ending uncut is %d bytes (%v), want it over the budget for the test to mean anything", n, err)
	}
	atHalf := cardBuilder{caps: capabilities(), cut: half}
	if n, err := size(cardActivity(atHalf.ending(e))); err != nil || n > maxMessageBytes {
		t.Fatalf("the ending at half the cap is %d bytes (%v), want it within the budget", n, err)
	}
	card := cardOf(t, mustRender(t, chat.End(e)))
	longest := 0
	for _, line := range outline(t, card) {
		longest = max(longest, chat.Units(strings.TrimLeft(strings.TrimPrefix(line, "text:"), "*~`")))
	}
	if longest != half {
		t.Errorf("the longest text of the ending is %d units, want %d: cut to half the cap and no further", longest, half)
	}
}
