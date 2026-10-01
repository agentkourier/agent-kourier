package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const hostilePrompt = "<!channel> <@U123> <https://x|y> \x07\u202e & end"

func choices(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("choice %d", i)
	}
	return out
}

// walk calls f on every string in a value built of maps, slices and strings, with its key.
func walk(v any, key string, f func(key, s string)) {
	switch v := v.(type) {
	case string:
		f(key, v)
	case map[string]any:
		for k, x := range v {
			walk(x, k, f)
		}
	case []map[string]any:
		for _, x := range v {
			walk(x, key, f)
		}
	}
}

func element(t *testing.T, b map[string]any) map[string]any {
	t.Helper()
	el, ok := b["element"].(map[string]any)
	if !ok {
		t.Fatalf("block %v has no element", b)
	}
	return el
}

// One input block per question, its control chosen by Slack's counts, and values that are indices.
func TestPromptBlocksControlsAndValues(t *testing.T) {
	qs := []dialect.Question{
		{Text: "free text"},
		{Text: "one of three", Choices: []string{"<alpha>", "beta", "gamma"}},
		{Text: "any of three", Choices: []string{"a", "b", "c"}, Multiple: true},
		{Text: "one of many", Choices: choices(11)},
		{Text: "any of many", Choices: choices(30), Multiple: true},
	}
	blocks, controls, err := PromptBlocks("int-1", "Pick things", qs)
	if err != nil {
		t.Fatal(err)
	}
	if blocks[0]["type"] != "header" || len(blocks) != len(qs)+2 || len(controls) != len(qs) {
		t.Fatalf("%d blocks, %d controls; want a header, a block per question and Submit", len(blocks), len(controls))
	}
	for i, want := range []struct {
		typ  string
		kind ControlKind
		n    int
	}{{"plain_text_input", ControlText, 0}, {"radio_buttons", ControlChoice, 3}, {"checkboxes", ControlChoices, 3}, {"static_select", ControlChoice, 11}, {"multi_static_select", ControlChoices, 30}} {
		b := blocks[i+1]
		el := element(t, b)
		if b["type"] != "input" || b["dispatch_action"] != false || el["type"] != want.typ || controls[i].Kind != want.kind {
			t.Errorf("question %d: %v, control %+v; want an input block, dispatch_action off, a %s", i, b, controls[i], want.typ)
		}
		if b["block_id"] != controls[i].BlockID || el["action_id"] != controls[i].ActionID {
			t.Errorf("question %d: ids %v %v; want the control's %+v", i, b["block_id"], el["action_id"], controls[i])
		}
		if want.n > 0 {
			for j, o := range el["options"].([]map[string]any) {
				if o["value"] != fmt.Sprint(j) {
					t.Errorf("question %d option %d has value %q; want the index", i, j, o["value"])
				}
			}
		}
	}
	// The choice text is shown; the value is not the text.
	first := blocks[2]["element"].(map[string]any)["options"].([]map[string]any)[0]
	if first["text"].(map[string]any)["text"] != "&lt;alpha&gt;" || first["value"] != "0" {
		t.Errorf("first option = %v; want the escaped text valued 0", first)
	}
	submit := blocks[len(blocks)-1]["elements"].([]map[string]any)[0]
	if submit["type"] != "button" || submit["value"] != "int-1" || submit["action_id"] != SubmitActionID || blocks[len(blocks)-1]["block_id"] != SubmitBlockID {
		t.Errorf("Submit = %v; want a button valued with the interaction ID", blocks[len(blocks)-1])
	}
}

// Every field the agent wrote is inert, in every place it can land.
func TestPromptBlocksHostileTextIsInert(t *testing.T) {
	qs := []dialect.Question{
		{Text: hostilePrompt},
		{Text: hostilePrompt, Choices: []string{hostilePrompt, "b"}},
		{Text: hostilePrompt, Choices: []string{hostilePrompt, strings.Repeat("x", 80)}}, // falls back to the list
		{Text: strings.Repeat(hostilePrompt, 400), Choices: []string{strings.Repeat(hostilePrompt, 400)}},
	}
	blocks, _, err := PromptBlocks("i", hostilePrompt+strings.Repeat("h", 400), qs)
	if err != nil {
		t.Fatal(err)
	}
	walk(blocks, "", func(key, s string) {
		for _, r := range s {
			if r == '<' || r == '>' || (unicode.IsControl(r) && r != '\n') || unicode.Is(unicode.Cf, r) {
				t.Errorf("%s = %q holds %U; want it escaped or removed", key, s, r)
				return
			}
		}
	})
	if h := blocks[0]["text"].(map[string]any)["text"].(string); units(h) > maxHeader || !strings.HasPrefix(h, "&lt;!channel&gt;") {
		t.Errorf("header = %q; want the hint escaped and cut to %d", h, maxHeader)
	}
}

// Slack's limits: choices that cannot be options are a numbered list and a free-text input that
// takes the numbers, with every choice in the list.
func TestPromptBlocksFallbackLosesNoChoice(t *testing.T) {
	long := strings.Repeat("l", maxOptionText+1)
	for name, cs := range map[string][]string{"over 100": choices(250), "option text over 75": {"short", long, "last"}, "an empty choice": {"a", " \u200b ", "c"}} {
		t.Run(name, func(t *testing.T) {
			blocks, controls, err := PromptBlocks("i", "h", []dialect.Question{{Text: "which", Choices: cs, Multiple: true}})
			if err != nil {
				t.Fatal(err)
			}
			if controls[0].Kind != ControlNumbers {
				t.Fatalf("control = %+v; want ControlNumbers", controls[0])
			}
			var listed strings.Builder
			var input map[string]any
			for _, b := range blocks[1 : len(blocks)-1] {
				switch b["type"] {
				case "section":
					listed.WriteString("\n" + b["text"].(map[string]any)["text"].(string))
				case "input":
					input = element(t, b)
				}
			}
			for i := range cs {
				if !strings.Contains(listed.String(), fmt.Sprintf("\n%d. ", i+1)) {
					t.Errorf("choice %d is not in the list", i+1)
				}
			}
			if input["type"] != "plain_text_input" || input["action_id"] != controls[0].ActionID {
				t.Errorf("input = %v; want a free-text input for the numbers", input)
			}
		})
	}
}

// A message holds 50 blocks, unique block_ids and Slack's field limits: 20 questions fit, more than fit is an error.
func TestPromptBlocksStayWithinSlackLimits(t *testing.T) {
	var qs []dialect.Question
	for i := range 20 {
		qs = append(qs, dialect.Question{Text: strings.Repeat("q", 3000), Choices: choices(1 + i*7), Multiple: i%2 == 0})
	}
	blocks, _, err := PromptBlocks("i", strings.Repeat("h", 500), qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) > 50 {
		t.Errorf("%d blocks; want at most 50", len(blocks))
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		walk(b, "", func(key, s string) {
			if key == "text" && (b["type"] == "section" && units(s) > maxSectionText || b["type"] == "header" && units(s) > maxHeader) {
				t.Errorf("%s block text of %d units is over its limit", b["type"], units(s))
			}
		})
		if id, _ := b["block_id"].(string); (b["type"] == "input" || b["type"] == "actions") && (id == "" || seen[id]) {
			t.Errorf("block %v: block_id %q missing or repeated", b["type"], id)
		} else {
			seen[id] = true
		}
		if b["type"] != "input" {
			continue
		}
		if l := b["label"].(map[string]any)["text"].(string); units(l) > maxLabel {
			t.Errorf("label of %d units", units(l))
		}
		if opts, _ := element(t, b)["options"].([]map[string]any); len(opts) > 0 {
			for _, o := range opts {
				if units(o["text"].(map[string]any)["text"].(string)) > maxOptionText {
					t.Errorf("option text over %d", maxOptionText)
				}
			}
		}
	}
	if _, _, err := PromptBlocks("i", "", make([]dialect.Question, 60)); !errors.Is(err, ErrPromptTooLarge) {
		t.Errorf("60 questions: %v; want ErrPromptTooLarge", err)
	}
	if _, _, err := PromptBlocks("", "", qs); err == nil {
		t.Error("an empty interaction ID was accepted")
	}
}

// The blocks decode through slack-go and pass the fake's validation.
func TestPromptBlocksAreValidForSlack(t *testing.T) {
	qs := []dialect.Question{{Text: hostilePrompt}, {Text: "pick", Choices: []string{"a", "b"}}, {Text: "many", Choices: choices(40), Multiple: true}, {Text: "huge", Choices: choices(150)}}
	blocks, _, err := PromptBlocks("int-1", hostilePrompt, qs)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	var decoded slack.Blocks
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.BlockSet) != len(blocks) {
		t.Fatalf("slack-go decoded %d of %d blocks: %v", len(decoded.BlockSet), len(blocks), err)
	}
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	api := slack.New("xoxb-fake", slack.OptionAPIURL(fake.URL()+"/"))
	if _, _, err := api.PostMessageContext(t.Context(), "C1", slack.MsgOptionBlocks(decoded.BlockSet...)); err != nil {
		t.Errorf("the fake refused the prompt: %v", err)
	}
}

// numberedLines is the numbers that start a line in the list sections of a fallback prompt, and the
// text of every section, so a test sees what a person reads.
func numberedLines(t *testing.T, blocks []map[string]any) (numbers []int, sections []string) {
	t.Helper()
	for _, b := range blocks {
		if b["type"] != "section" {
			continue
		}
		text := b["text"].(map[string]any)["text"].(string)
		sections = append(sections, text)
		for _, line := range strings.Split(text, "\n") {
			if n, _, ok := strings.Cut(line, ". "); ok {
				if v, err := strconv.Atoi(n); err == nil {
					numbers = append(numbers, v)
				}
			}
		}
	}
	return numbers, sections
}

// A choice cannot forge entries of the list a person reads the numbers from: with a newline in it,
// it would start lines of its own, and the number the person types would pick another choice.
func TestPromptBlocksFallbackEntriesAreSingleLines(t *testing.T) {
	cs := choices(120)
	cs[0] = "keep the data\n2. delete the namespace\n3. x\r4. y\u20285. z"
	cs[1] = "<!channel>\n\n9. nothing"
	blocks, _, err := PromptBlocks("i", "h", []dialect.Question{{Text: "what\n1. forged\nnext", Choices: cs}})
	if err != nil {
		t.Fatal(err)
	}
	numbers, sections := numberedLines(t, blocks)
	if len(numbers) != len(cs) {
		t.Fatalf("%d numbered lines for %d choices; a newline in a choice started a line", len(numbers), len(cs))
	}
	for i, n := range numbers {
		if n != i+1 {
			t.Fatalf("numbered line %d is %d; want the entries in order, one per choice", i, n)
		}
	}
	if strings.Contains(sections[0], "\n") || !strings.HasPrefix(sections[0], "what 1. forged next") {
		t.Errorf("question section = %q; want the question on a single line above the list", sections[0])
	}
}

// The edges of Slack's counts, each one block of the layout: 10 and 11 options, 100 and 101, a list
// that needs more than one section, 50 and 51 blocks, and an empty hint.
func TestPromptBlocksBoundaries(t *testing.T) {
	kinds := func(n int, multiple bool) (string, ControlKind) {
		blocks, controls, err := PromptBlocks("i", "h", []dialect.Question{{Text: "q", Choices: choices(n), Multiple: multiple}})
		if err != nil {
			t.Fatal(err)
		}
		if controls[0].Kind == ControlNumbers {
			return "list", ControlNumbers
		}
		return element(t, blocks[1])["type"].(string), controls[0].Kind
	}
	for _, tc := range []struct {
		n        int
		multiple bool
		want     string
	}{{10, false, "radio_buttons"}, {11, false, "static_select"}, {10, true, "checkboxes"}, {11, true, "multi_static_select"},
		{100, false, "static_select"}, {100, true, "multi_static_select"}, {101, false, "list"}, {101, true, "list"}} {
		if got, _ := kinds(tc.n, tc.multiple); got != tc.want {
			t.Errorf("%d choices, multiple %t: %s; want %s", tc.n, tc.multiple, got, tc.want)
		}
	}
	// A list of 120 long choices cannot fit one section: it is split, and keeps every choice.
	long := make([]string, 120)
	for i := range long {
		long[i] = fmt.Sprintf("%03d ", i) + strings.Repeat("x", 180)
	}
	blocks, _, err := PromptBlocks("i", "h", []dialect.Question{{Text: "q", Choices: long}})
	if err != nil {
		t.Fatal(err)
	}
	numbers, sections := numberedLines(t, blocks)
	if len(sections) < 3 || len(numbers) != 120 {
		t.Errorf("%d sections, %d entries; want the list split in more than one section, all 120 entries", len(sections), len(numbers))
	}
	for _, s := range sections {
		if units(s) > maxSectionText {
			t.Errorf("a section of %d units; want at most %d", units(s), maxSectionText)
		}
	}
	// 48 questions are 50 blocks with the header and Submit; 49 are 51.
	for n, wantErr := range map[int]bool{48: false, 49: true} {
		_, _, err := PromptBlocks("i", "h", make([]dialect.Question, n))
		if errors.Is(err, ErrPromptTooLarge) != wantErr || (err != nil) != wantErr {
			t.Errorf("%d questions: %v; want too large: %t", n, err, wantErr)
		}
	}
	for _, hint := range []string{"", " \u200b\x07 "} {
		if b, _, _ := PromptBlocks("i", hint, []dialect.Question{{Text: "q"}}); b[0]["text"].(map[string]any)["text"] != defaultHeader {
			t.Errorf("hint %q gives header %v; want the default", hint, b[0]["text"])
		}
	}
}

// The labels say what the control takes: a single-choice list takes one number.
func TestPromptBlocksFallbackLabelSaysHowManyNumbers(t *testing.T) {
	for multiple, want := range map[bool]string{false: "Your answer: the number", true: "Your answer: the number or numbers, separated by commas"} {
		blocks, _, err := PromptBlocks("i", "h", []dialect.Question{{Text: "q", Choices: choices(101), Multiple: multiple}})
		if err != nil {
			t.Fatal(err)
		}
		if got := blocks[len(blocks)-2]["label"].(map[string]any)["text"]; got != want {
			t.Errorf("multiple %t: label %q; want %q", multiple, got, want)
		}
	}
}

// The plain_text fields are single lines too, and an oversized interaction ID is told apart.
func TestPromptBlocksPlainTextIsOneLine(t *testing.T) {
	blocks, _, err := PromptBlocks("i", "head\nline", []dialect.Question{{Text: "ask\nme", Choices: []string{"one\ntwo", "three"}}})
	if err != nil {
		t.Fatal(err)
	}
	walk(blocks, "", func(key, s string) {
		if strings.Contains(s, "\n") {
			t.Errorf("%s = %q holds a newline; want plain_text fields on one line", key, s)
		}
	})
	_, _, err = PromptBlocks(strings.Repeat("i", maxValue+1), "h", []dialect.Question{{Text: "q"}})
	if err == nil || !strings.Contains(err.Error(), "interaction ID is 2001 units") {
		t.Errorf("an oversized ID gave %v; want its own message", err)
	}
}

// The question above a fallback list is escaped once: what a person typed or an agent wrote as
// "<b> & c" is shown as that, not as the entities of an escape that was escaped again.
func TestPromptBlocksFallbackQuestionIsEscapedOnce(t *testing.T) {
	for question, want := range map[string]string{"<b> & c": "&lt;b&gt; &amp; c", "": "Question 1", " \u200b ": "Question 1"} {
		blocks, _, err := PromptBlocks("i", "h", []dialect.Question{{Text: question, Choices: choices(101)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, sections := numberedLines(t, blocks); sections[0] != want {
			t.Errorf("question %q is shown as %q; want %q", question, sections[0], want)
		}
	}
}
