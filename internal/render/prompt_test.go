package render

import (
	"encoding/json"
	"errors"
	"fmt"
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
					listed.WriteString(b["text"].(map[string]any)["text"].(string))
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
