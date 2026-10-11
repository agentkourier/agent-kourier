package slack_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The layout of a tool approval (spec 8.4): one tool gets an Approve and a Deny button; several get one
// Approve/Deny radio each, a Submit button, and Approve all and Deny all.

const approvalID = "ix-0123456789abcdef"

func approvalOf(tools ...chat.Tool) chat.Prompt { return chat.Prompt{ID: approvalID, Tools: tools} }

func manyTools(n int) []chat.Tool {
	out := make([]chat.Tool, n)
	for i := range out {
		out[i] = chat.Tool{Name: fmt.Sprintf("tool_%d", i), Args: fmt.Sprintf(`{"n":%d}`, i)}
	}
	return out
}

// buttonsOf are the elements of an actions block.
func buttonsOf(b map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range asList(b["elements"]) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func labelOf(b map[string]any) string {
	l, _ := b["label"].(map[string]any)
	s, _ := l["text"].(string)
	return s
}

// plainText is the text of the children of a rich_text element, which must all be plain text elements: a mention, a link or a
// style would act.
func plainText(t *testing.T, el map[string]any) string {
	t.Helper()
	var out strings.Builder
	for _, e := range asList(el["elements"]) {
		c := e.(map[string]any)
		if c["type"] != "text" || len(c) != 2 {
			t.Fatalf("element %v of %v is not plain text", c, el["type"])
		}
		out.WriteString(c["text"].(string))
	}
	return out.String()
}

// richParts reads a rich_text block of a tool: the name in its section, if the block has a name, and the arguments in its
// preformatted element, or none if it says there are none.
func richParts(t *testing.T, b map[string]any) (name, args string, none bool) {
	t.Helper()
	els := asList(b["elements"])
	if b["type"] != "rich_text" || len(els) == 0 || len(els) > 2 {
		t.Fatalf("block %v is not a rich_text block of a tool", b)
	}
	if len(els) == 2 {
		first := els[0].(map[string]any)
		if first["type"] != "rich_text_section" {
			t.Fatalf("the first element of %v is not the name's section", b)
		}
		name = plainText(t, first)
	}
	last := els[len(els)-1].(map[string]any)
	switch last["type"] {
	case "rich_text_preformatted":
		args = plainText(t, last)
	case "rich_text_section":
		if none = plainText(t, last) == "(no arguments)"; !none {
			t.Fatalf("section %v says neither the name nor that there are no arguments", last)
		}
	default:
		t.Fatalf("element %v is neither the arguments nor the line that says there are none", last)
	}
	return name, args, none
}

// argsOf is the arguments of the tool of a rich_text block, as preformatted text.
func argsOf(t *testing.T, b map[string]any) string {
	t.Helper()
	_, args, none := richParts(t, b)
	if none {
		t.Fatalf("block %v has no arguments", b)
	}
	return args
}

// unescaped is a plain_text object's text as Slack shows it: the entities read.
var unescaped = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

func TestOneToolGetsAnApproveButtonAndADenyButton(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(chat.Tool{Name: "kubectl_delete", Args: `{"name":"web"}`})
	p.Hint = "Delete the pod?"

	blocks := w.post(t, chat.Ask(p))
	m, _ := w.last(t)

	if got := strings.Join(types(blocks), ","); got != "header,section,rich_text,actions" {
		t.Fatalf("blocks = %s, want header, the tool's name, its arguments, and the buttons", got)
	}
	if textOf(blocks[0]) != "Delete the pod?" || textOf(blocks[1]) != "kubectl_delete" || argsOf(t, blocks[2]) != `{"name":"web"}` {
		t.Errorf("header %q, name %q, arguments %q; want the hint, the name, and the arguments as preformatted text", textOf(blocks[0]), textOf(blocks[1]), argsOf(t, blocks[2]))
	}
	if blocks[1]["text"].(map[string]any)["type"] != "plain_text" {
		t.Errorf("the name is a %v object, want plain_text", blocks[1]["text"].(map[string]any)["type"])
	}
	btns := buttonsOf(blocks[3])
	if len(btns) != 2 || blocks[3]["block_id"] != "decide" {
		t.Fatalf("actions = %v, want two buttons in the block decide", blocks[3])
	}
	for i, want := range []struct{ id, label, style string }{{"approve_all", "Approve", "primary"}, {"deny_all", "Deny", "danger"}} {
		if btns[i]["action_id"] != want.id || btns[i]["value"] != approvalID || textOf(btns[i]) != want.label || btns[i]["style"] != want.style {
			t.Errorf("button %d = %v, want %+v valued with the prompt's ID", i, btns[i], want)
		}
	}
	if m.Text != "The agent needs your approval." {
		t.Errorf("text = %q, want Agent Kourier's own words", m.Text)
	}
}

func TestSeveralToolsGetARadioEachASubmitAndTheTwoAllButtons(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(manyTools(3)...)

	blocks := w.post(t, chat.Ask(p))

	if got := strings.Join(types(blocks), ","); got != "header,rich_text,input,rich_text,input,rich_text,input,actions" {
		t.Fatalf("blocks = %s, want the header, then the name with the arguments, and the control, of each tool, then the buttons", got)
	}
	if textOf(blocks[0]) != "The agent needs your approval" {
		t.Errorf("header with no hint = %q, want the default for an approval", textOf(blocks[0]))
	}
	for i := range 3 {
		tool, in := blocks[1+2*i], blocks[2+2*i]
		el := element(in)
		name, args, _ := richParts(t, tool)
		if args != fmt.Sprintf(`{"n":%d}`, i) || name != fmt.Sprintf("%d. tool_%d", i+1, i) {
			t.Errorf("tool %d block holds the name %q and the arguments %q", i, name, args)
		}
		if in["block_id"] != fmt.Sprintf("q%d", i) || in["dispatch_action"] == true || labelOf(in) != name ||
			el["type"] != "radio_buttons" || el["action_id"] != fmt.Sprintf("r%d", i) {
			t.Errorf("tool %d control = %v, want a radio in the block q%d with the tool's name as its label and dispatch_action off", i, in, i)
		}
		opts := asList(el["options"])
		if len(opts) != 2 || opts[0].(map[string]any)["value"] != chat.ToolApproveValue || opts[1].(map[string]any)["value"] != chat.ToolDenyValue ||
			textOf(opts[0].(map[string]any)) != "Approve" || textOf(opts[1].(map[string]any)) != "Deny" {
			t.Errorf("tool %d options = %v, want Approve valued %q and Deny valued %q", i, opts, chat.ToolApproveValue, chat.ToolDenyValue)
		}
		if el["initial_option"] != nil {
			t.Errorf("tool %d radio has an initial option: nothing may be decided before a person decides it", i)
		}
	}
	btns := buttonsOf(blocks[7])
	if blocks[7]["block_id"] != "submit" || len(btns) != 3 {
		t.Fatalf("actions = %v, want three buttons in the block submit", blocks[7])
	}
	for i, want := range []struct{ id, label string }{{"submit", "Submit"}, {"approve_all", "Approve all"}, {"deny_all", "Deny all"}} {
		if btns[i]["action_id"] != want.id || btns[i]["value"] != approvalID || textOf(btns[i]) != want.label {
			t.Errorf("button %d = %v, want %+v valued with the prompt's ID", i, btns[i], want)
		}
	}
}

// Every block that takes part in a press has an explicit ID of its own, and no action ID is used twice in a message.
func TestEveryInteractiveBlockOfAnApprovalHasItsOwnIDs(t *testing.T) {
	w := newWorld(t)
	for _, n := range []int{1, 2, 20} {
		blocks := w.post(t, chat.Ask(approvalOf(manyTools(n)...)))
		blockIDs, actionIDs := map[string]bool{}, map[string]bool{}
		for _, b := range blocks {
			if b["type"] != "input" && b["type"] != "actions" {
				continue
			}
			id, _ := b["block_id"].(string)
			if id == "" || blockIDs[id] {
				t.Errorf("%d tools: block_id %q is empty or used twice", n, id)
			}
			blockIDs[id] = true
			var els []map[string]any
			if e := element(b); e != nil {
				els = append(els, e)
			}
			els = append(els, buttonsOf(b)...)
			for _, e := range els {
				aid, _ := e["action_id"].(string)
				if aid == "" || actionIDs[aid] {
					t.Errorf("%d tools: action_id %q is empty or used twice", n, aid)
				}
				actionIDs[aid] = true
			}
		}
	}
}

// 20 tools are the most a pause carries. At two blocks for each, a header, the sub-agent's line, the deadline's
// and the buttons, they take 44 of the 50 a message holds.
func TestTwentyToolsWithASubAgentAndADeadlineFitInFiftyBlocks(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(manyTools(20)...)
	p.Subagent, p.Deadline = "sre-agent", time.Date(2026, 10, 11, 15, 4, 0, 0, time.UTC)

	if err := w.p.Fits(p); err != nil {
		t.Fatalf("Fits of 20 tools = %v", err)
	}
	blocks := w.post(t, chat.Ask(p))

	if len(blocks) != 44 {
		t.Errorf("%d blocks, want 44: header, sub-agent, 40 for the tools, deadline, buttons", len(blocks))
	}
	if blocks[1]["type"] != "context" || blocks[len(blocks)-2]["type"] != "context" || blocks[len(blocks)-1]["type"] != "actions" {
		t.Errorf("block types %v, want the sub-agent's line under the header, the deadline above the buttons, and the buttons last", types(blocks))
	}
	if sub := blocks[1]["elements"].([]any)[0].(map[string]any)["text"]; sub != "Raised by sub-agent sre-agent" {
		t.Errorf("sub-agent line = %v", sub)
	}
}

// More tools than 50 blocks hold are too large; the optional lines are left out of a prompt that has no room for them.
func TestAPromptOfToolsThatFillsTheBlocksLeavesOutTheOptionalLines(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct {
		name string
		n    int
		want error
	}{{"24 tools take exactly 50", 24, nil}, {"25 tools take 52", 25, chat.ErrTooLarge}, {"far too many", 500, chat.ErrTooLarge}} {
		if err := w.p.Fits(approvalOf(manyTools(tc.n)...)); !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("%s: Fits = %v, want %v", tc.name, err, tc.want)
		}
	}
	full := approvalOf(manyTools(24)...)
	full.Subagent, full.Deadline = "sre", time.Now().Add(time.Hour)
	blocks := w.post(t, chat.Ask(full))
	if len(blocks) != 50 || blocks[1]["type"] != "rich_text" || blocks[len(blocks)-1]["type"] != "actions" {
		t.Errorf("a prompt of 50 blocks has %d, %v after the header; want the optional lines left out", len(blocks), types(blocks)[:3])
	}
	roomy := approvalOf(manyTools(23)...)
	roomy.Subagent, roomy.Deadline = "sre", time.Now().Add(time.Hour)
	if got := w.post(t, chat.Ask(roomy)); len(got) != 50 || got[1]["type"] != "context" || got[len(got)-2]["type"] != "context" {
		t.Errorf("a prompt with room for both lines has %d blocks and %v after the header; want 50 and both lines", len(got), got[1]["type"])
	}
	one := approvalOf(manyTools(24)...)
	one.Deadline = time.Now().Add(time.Hour)
	if got := w.post(t, chat.Ask(one)); len(got) != 50 || got[len(got)-2]["type"] == "context" {
		t.Errorf("a prompt of 50 blocks with a deadline has %d, and %v above the buttons; want the deadline's line left out", len(got), got[len(got)-2]["type"])
	}
}

func TestAnApprovalSaysWhenItCloses(t *testing.T) {
	w := newWorld(t)
	deadline := time.Date(2026, 10, 11, 15, 4, 0, 0, time.UTC)
	for name, tools := range map[string][]chat.Tool{"one tool": manyTools(1), "several": manyTools(2)} {
		p := approvalOf(tools...)
		p.Deadline = deadline
		blocks := w.post(t, chat.Ask(p))
		m, _ := w.last(t)
		line := blocks[len(blocks)-2]["elements"].([]any)[0].(map[string]any)
		if line["type"] != "mrkdwn" || line["verbatim"] != true || !strings.Contains(line["text"].(string), fmt.Sprintf("<!date^%d^", deadline.Unix())) {
			t.Errorf("%s: deadline line = %v, want a date token built from the time", name, line)
		}
		if blocks[len(blocks)-1]["type"] != "actions" || m.Text != "The agent needs your approval. Answer by 2026-10-11 15:04 UTC." {
			t.Errorf("%s: last block %v, text %q; want the buttons last and the deadline in the text", name, blocks[len(blocks)-1]["type"], m.Text)
		}
	}
}

// A tool with no name, or a name that cleans to nothing, is still told apart and decided; one with no arguments says so.
func TestAToolWithNoNameOrNoArgumentsIsStillShown(t *testing.T) {
	w := newWorld(t)

	one := w.post(t, chat.Ask(approvalOf(chat.Tool{Name: "\u200b \n", Args: " \n\t "})))
	if got := strings.Join(types(one), ","); got != "header,section,rich_text,actions" {
		t.Fatalf("blocks = %s, want a line in place of the arguments", got)
	}
	if _, _, none := richParts(t, one[2]); textOf(one[1]) != "Tool 1" || !none {
		t.Errorf("name %q, arguments %v; want the placeholders", textOf(one[1]), one[2])
	}

	several := w.post(t, chat.Ask(approvalOf(chat.Tool{Args: "{}"}, chat.Tool{Name: "ping"}, chat.Tool{Name: "<\u200b>", Args: "{}"})))
	if got := strings.Join(types(several), ","); got != "header,rich_text,input,rich_text,input,rich_text,input,actions" {
		t.Fatalf("blocks = %s", got)
	}
	if name, _, none := richParts(t, several[3]); !none || name != "2. ping" {
		t.Errorf("the tool with no arguments is %q, none %t; want its numbered name and the line that says so", name, none)
	}
	if labelOf(several[2]) != "Tool 1" || labelOf(several[4]) != "2. ping" || labelOf(several[6]) != "3. &lt;&gt;" {
		t.Errorf("labels %q %q %q, want the placeholder for a tool with no name, and the escaped name of the rest", labelOf(several[2]), labelOf(several[4]), labelOf(several[6]))
	}
}

// The name is plain_text and escaped. The arguments are the characters of a preformatted text element, cleaned, with
// nothing in the block but that element: no token, link, fence or backtick of them is anything but text.
func TestToolTextIsInert(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct{ name, args, want string }{
		{"tokens and links", "<!channel> <@U0123456789> <#C1> [x](https://evil.test) a < b <b> & 2>&1 &lt;!here&gt;",
			"<!channel> <@U0123456789> <#C1> [x](https://evil.test) a < b <b> & 2>&1 &lt;!here&gt;"},
		{"backticks and tildes", "a `tick` ``two`` ~~~ ```", "a `tick` ``two`` ~~~ ```"},
		{"lines that are fences", "{\n```\n  ~~~~ x\n\t```bash\n}", "{\n```\n  ~~~~ x\n\t```bash\n}"},
		{"control and hidden characters", "a\x07b\u200bc\u202ed\r\ne", "abcd\ne"},
		{"emphasis, a heading and a quote", "*b* _i_\n# h\n> q\n- l", "*b* _i_\n# h\n> q\n- l"},
	} {
		blocks := w.post(t, chat.Ask(approvalOf(chat.Tool{Name: "*b* <!channel> `c` &", Args: tc.args}, chat.Tool{Name: "other", Args: "{}"})))
		name, args, _ := richParts(t, blocks[1])
		if args != tc.want {
			t.Errorf("%s: arguments are %q, want %q", tc.name, args, tc.want)
		}
		if label := labelOf(blocks[2]); label != "1. *b* &lt;!channel&gt; `c` &amp;" || name != "1. *b* <!channel> `c` &" {
			t.Errorf("%s: name label = %q and name text %q, want the label escaped as plain_text and the text literal", tc.name, label, name)
		}
	}
}

// A prompt that does not fit is refused by Fits, Post and Update alike, and sends nothing.
func TestAToolThatDoesNotFitIsRefusedAndNeverCut(t *testing.T) {
	w := newWorld(t)
	limit := w.p.Capabilities().Limit(chat.WhereToolArgs)
	nameLimit := w.p.Capabilities().Limit(chat.WhereToolName)
	if limit <= 0 || limit > 12000 {
		t.Fatalf("the limit of the arguments is %d", limit)
	}
	for _, tc := range []struct {
		name string
		tool chat.Tool
	}{
		{"a name over the limit", chat.Tool{Name: strings.Repeat("n", nameLimit+1), Args: "{}"}},
		{"arguments over the limit", chat.Tool{Name: "t", Args: strings.Repeat("a", limit+1)}},
		{"arguments of two-unit characters over the limit", chat.Tool{Name: "t", Args: strings.Repeat("\U0001F389", limit/2+1)}},
	} {
		p := approvalOf(chat.Tool{Name: "first", Args: "{}"}, tc.tool)
		before := len(w.fake.Thread("C1", w.target.Thread))
		if err := w.p.Fits(p); !errors.Is(err, chat.ErrTooLarge) {
			t.Errorf("%s: Fits = %v, want ErrTooLarge", tc.name, err)
		}
		if _, err := w.p.Post(t.Context(), w.target, chat.Ask(p), "tag-"+tc.name); !errors.Is(err, chat.ErrTooLarge) {
			t.Errorf("%s: Post = %v, want ErrTooLarge", tc.name, err)
		}
		seed, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "seed"}), "seed-"+tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.p.Update(t.Context(), seed, chat.Ask(p)); !errors.Is(err, chat.ErrTooLarge) {
			t.Errorf("%s: Update = %v, want ErrTooLarge", tc.name, err)
		}
		if after := len(w.fake.Thread("C1", w.target.Thread)); after != before+1 {
			t.Errorf("%s: the thread has %d messages, want %d: only the seed", tc.name, after, before+1)
		}
	}
}

// Text exactly at the limit fits and is shown whole, whatever it is made of.
func TestTextAtTheLimitIsShownWhole(t *testing.T) {
	w := newWorld(t)
	limit := w.p.Capabilities().Limit(chat.WhereToolArgs)
	nameLimit := w.p.Capabilities().Limit(chat.WhereToolName)
	for name, args := range map[string]string{
		"plain":               strings.Repeat("a", limit),
		"tokens":              strings.Repeat("<!", limit/2),
		"fence lines":         strings.Repeat("```\n", limit/4),
		"two-unit characters": strings.Repeat("\U0001F389", limit/2),
		"entities":            strings.Repeat("&", limit),
	} {
		if chat.Units(args) > limit {
			t.Fatalf("%s: the test text is %d units, over the limit of %d", name, chat.Units(args), limit)
		}
		// The name is numbered, and its number counts: "1. " and the name are the 300 units of a name.
		p := approvalOf(chat.Tool{Name: strings.Repeat("&", nameLimit-3), Args: args}, chat.Tool{Name: "other", Args: "{}"})
		blocks := w.post(t, chat.Ask(p))
		if got := argsOf(t, blocks[1]); got != chat.Clean(args, false) {
			t.Errorf("%s: the arguments are cut or changed: %d units shown of %d", name, chat.Units(got), chat.Units(args))
		}
		if label := labelOf(blocks[2]); strings.Contains(label, chat.Truncated) || label != "1. "+strings.Repeat("&amp;", nameLimit-3) {
			t.Errorf("%s: the name at the limit is cut or not escaped: %.60q", name, label)
		}
	}
}

// A limit is a limit of the text as it is shown, cleaned and numbered: text that is over it raw and cleans to exactly it
// fits, the unit after it does not, and the total of a message is the sum of the cleaned arguments.
func TestTheLimitsAreMeasuredOnTheTextAsShown(t *testing.T) {
	w := newWorld(t)
	limit := w.p.Capabilities().Limit(chat.WhereToolArgs)
	nameLimit := w.p.Capabilities().Limit(chat.WhereToolName)
	hidden := func(n int) string { return strings.Repeat("a\u200b", n) } // n units that clean to n, in 2n raw
	fits := func(p chat.Prompt) bool {
		err := w.p.Fits(p)
		if err != nil && !errors.Is(err, chat.ErrTooLarge) {
			t.Fatalf("Fits = %v", err)
		}
		return err == nil
	}
	pad := func(n int, last chat.Tool) []chat.Tool { return append(manyTools(n), last) } // n small tools, then last
	for _, tc := range []struct {
		name string
		p    chat.Prompt
		want bool
	}{
		{"arguments that clean to the limit", approvalOf(chat.Tool{Name: "t", Args: hidden(limit)}), true},
		{"arguments that clean to one over", approvalOf(chat.Tool{Name: "t", Args: hidden(limit + 1)}), false},
		{"a name of one tool that cleans to the limit", approvalOf(chat.Tool{Name: hidden(nameLimit), Args: "{}"}), true},
		{"a name of one tool that cleans to one over", approvalOf(chat.Tool{Name: hidden(nameLimit + 1), Args: "{}"}), false},
		{"a name of the first of two that, with its number, is the limit", approvalOf(chat.Tool{Name: hidden(nameLimit - 3)}, chat.Tool{Name: "u"}), true},
		{"a name of the first of two that, with its number, is one over", approvalOf(chat.Tool{Name: hidden(nameLimit - 2)}, chat.Tool{Name: "u"}), false},
		{"a name of the 12th of 12 that, with its number, is the limit", approvalOf(pad(11, chat.Tool{Name: hidden(nameLimit - 4)})...), true},
		{"a name of the 12th of 12 that, with its number, is one over", approvalOf(pad(11, chat.Tool{Name: hidden(nameLimit - 3)})...), false},
		{"four tools of the limit's worth of arguments, which are 12000", approvalOf(
			chat.Tool{Name: "a", Args: hidden(limit)}, chat.Tool{Name: "b", Args: hidden(limit)}, chat.Tool{Name: "c", Args: hidden(limit)}, chat.Tool{Name: "d", Args: hidden(limit)}), true},
		{"the same and one unit more, which are 12001", approvalOf(
			chat.Tool{Name: "a", Args: hidden(limit)}, chat.Tool{Name: "b", Args: hidden(limit)}, chat.Tool{Name: "c", Args: hidden(limit)}, chat.Tool{Name: "d", Args: hidden(limit)}, chat.Tool{Name: "e", Args: "x"}), false},
	} {
		if got := fits(tc.p); got != tc.want {
			t.Errorf("%s: Fits = %t, want %t", tc.name, got, tc.want)
		}
	}
	if limit*4 != 12000 {
		t.Errorf("the limit is %d: the test of the total assumes four tools of it make 12000", limit)
	}
}

// A message shows the arguments of all its tools up to a total. Tools that together need more are too large, and none is cut.
func TestToolsThatTogetherExceedWhatAMessageHoldsAreTooLarge(t *testing.T) {
	w := newWorld(t)
	each := w.p.Capabilities().Limit(chat.WhereToolArgs) - 1
	big := func(n int) []chat.Tool {
		out := make([]chat.Tool, n)
		for i := range out {
			out[i] = chat.Tool{Name: fmt.Sprintf("t%d", i), Args: strings.Repeat("x", each)}
		}
		return out
	}
	var fits int
	for n := 1; n <= 20; n++ {
		if err := w.p.Fits(approvalOf(big(n)...)); err != nil {
			if !errors.Is(err, chat.ErrTooLarge) {
				t.Fatalf("%d tools: Fits = %v", n, err)
			}
			break
		}
		fits = n
	}
	if fits < 2 || fits >= 20 {
		t.Fatalf("%d tools of %d units fit, want at least 2 and not all 20", fits, each)
	}
	blocks := w.post(t, chat.Ask(approvalOf(big(fits)...)))
	total := 0
	for _, b := range blocks {
		if b["type"] == "rich_text" {
			total += chat.Units(argsOf(t, b))
		}
	}
	if total != fits*each || total > 12000 {
		t.Errorf("%d units of arguments in a message of %d tools of %d, want all of them and at most 12000", total, fits, each)
	}
	if _, err := w.p.Post(t.Context(), w.target, chat.Ask(approvalOf(big(fits+1)...)), "tag-over"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post of %d tools = %v, want ErrTooLarge", fits+1, err)
	}
}

func TestPostAndFitsAgreeOnAPromptOfTools(t *testing.T) {
	w := newWorld(t)
	for _, p := range []chat.Prompt{
		approvalOf(manyTools(1)...), approvalOf(manyTools(20)...), approvalOf(manyTools(25)...),
		approvalOf(chat.Tool{Name: "t", Args: strings.Repeat("a", 5000)}),
		approvalOf(chat.Tool{Name: "t", Args: strings.Repeat("a", 2900)}, chat.Tool{Name: "u", Args: strings.Repeat("a", 2900)}, chat.Tool{Name: "v", Args: strings.Repeat("a", 2900)}, chat.Tool{Name: "w", Args: strings.Repeat("a", 2900)}, chat.Tool{Name: "x", Args: strings.Repeat("a", 2900)}),
	} {
		fits := w.p.Fits(p)
		_, post := w.p.Post(t.Context(), w.target, chat.Ask(p), fmt.Sprintf("tag-%d", len(p.Tools)+len(p.Tools[0].Args)))
		if (fits == nil) != (post == nil) || errors.Is(fits, chat.ErrTooLarge) != errors.Is(post, chat.ErrTooLarge) {
			t.Errorf("%d tools: Fits = %v, Post = %v; want the same answer", len(p.Tools), fits, post)
		}
	}
}

// The ending of a prompt of tools keeps what was asked in the thread: each tool, its arguments, and what was decided
// of it, with who decided and when as for any ending, and no control.
func TestTheEndingOfAnApprovalShowsEachToolItsArgumentsAndItsVerdict(t *testing.T) {
	w := newWorld(t)
	at := time.Date(2026, 10, 11, 15, 4, 0, 0, time.UTC)
	p := approvalOf(chat.Tool{Name: "kubectl_delete", Args: `{"name":"web"}`}, chat.Tool{Name: "ping"}, chat.Tool{Args: "<!channel>"})
	p.Subagent, p.Hint = "sre", "Clean up?"
	verdicts := []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied, chat.ToolApproved}

	blocks := w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, By: "U0123456789", At: at, Prompt: p, Verdicts: verdicts}))
	m, _ := w.last(t)

	status := blocks[0]["text"].(map[string]any)["text"].(string)
	if m.Text != "Answered" || !strings.HasPrefix(status, "*Answered* by <@U0123456789> <!date^") {
		t.Errorf("text %q, status %q; want it answered by the verified user, with the date", m.Text, status)
	}
	if got := strings.Join(types(blocks), ","); got != "section,context,context,section,rich_text,section,rich_text,section,rich_text" {
		t.Fatalf("blocks = %s, want the status, the sub-agent and the hint, then a name and its arguments for each tool", got)
	}
	for i, want := range []struct{ name, verdict, args string }{
		{"1. kubectl_delete", "Approved", `{"name":"web"}`}, {"2. ping", "Denied", ""}, {"Tool 3", "Approved", "<!channel>"},
	} {
		name, args := blocks[3+2*i], blocks[4+2*i]
		fields := asList(name["fields"])
		if textOf(name) != want.name || len(fields) != 1 || fields[0].(map[string]any)["text"] != want.verdict {
			t.Errorf("tool %d: name %q, fields %v; want %q with %q in its fields", i, textOf(name), fields, want.name, want.verdict)
		}
		if _, got, none := richParts(t, args); got != want.args || none != (want.args == "") {
			t.Errorf("tool %d: arguments %q (none: %t), want %q", i, got, none, want.args)
		}
	}
	for _, b := range blocks {
		if b["type"] == "actions" || b["type"] == "input" {
			t.Errorf("block %v is left in the ending: no control remains", b["type"])
		}
	}
}

func TestTheEndingOfAnApprovalThatNobodyDecidedShowsTheToolsAndNoVerdict(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(chat.Tool{Name: "a", Args: "{}"}, chat.Tool{Name: "b", Args: "{}"})
	for _, o := range []chat.Outcome{chat.TimedOut, chat.Cancelled} {
		blocks := w.post(t, chat.End(chat.Ending{Outcome: o, Prompt: p}))
		if got := strings.Join(types(blocks), ","); got != "section,section,rich_text,section,rich_text" {
			t.Errorf("%s: blocks = %s", o, got)
		}
		for _, b := range blocks[1:] {
			if b["fields"] != nil {
				t.Errorf("%s: %v shows a verdict nobody gave", o, b)
			}
		}
	}
}

// An ending is never refused for size. A name and arguments over the limits of their places are cut and marked,
// and the arguments of all the tools together are kept to what a message shows.
func TestTheEndingOfAnApprovalIsCutAndNeverRefused(t *testing.T) {
	w := newWorld(t)
	limit := w.p.Capabilities().Limit(chat.WhereToolArgs)
	nameLimit := w.p.Capabilities().Limit(chat.WhereToolName)
	huge := chat.Tool{Name: strings.Repeat("n", 5000), Args: strings.Repeat("a", 50000)}
	blocks := w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, Prompt: approvalOf(huge, huge), Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied}}))
	name, args := textOf(blocks[1]), argsOf(t, blocks[2])
	if !strings.HasSuffix(name, chat.Truncated) || chat.Units(name) > nameLimit {
		t.Errorf("the name is %d units %.40q, want it cut to %d and marked", chat.Units(name), name, nameLimit)
	}
	if !strings.HasSuffix(args, chat.Truncated) || chat.Units(args) > limit {
		t.Errorf("the arguments are %d units, want them cut to %d and marked", chat.Units(args), limit)
	}

	// 20 tools, each with all the arguments a tool may have: more than a message holds, so each is cut to its share.
	big := make([]chat.Tool, 20)
	verdicts := make([]chat.ToolVerdict, 20)
	for i := range big {
		big[i] = chat.Tool{Name: fmt.Sprintf("t%d", i), Args: strings.Repeat("x", limit)}
		verdicts[i] = chat.ToolDenied
	}
	blocks = w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, Prompt: approvalOf(big...), Verdicts: verdicts}))
	total := 0
	for _, b := range blocks {
		if b["type"] == "rich_text" {
			shown := argsOf(t, b)
			total += chat.Units(shown)
			if !strings.Contains(shown, chat.Truncated) {
				t.Errorf("an argument of %d units is shown whole beside 19 others in 12,000", limit)
			}
		}
	}
	if total > 12000 || len(blocks) != 41 {
		t.Errorf("%d units of arguments in %d blocks, want at most 12000 in 41", total, len(blocks))
	}
}

// A prompt that fits is shown whole in its ending, the large tool as much as the small one.
func TestTheEndingOfAnApprovalThatFitsShowsAllOfIt(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(chat.Tool{Name: "big", Args: strings.Repeat("x", 3000)}, chat.Tool{Name: "big2", Args: strings.Repeat("y", 3000)}, chat.Tool{Name: "small", Args: "{}"})
	if err := w.p.Fits(p); err != nil {
		t.Fatal(err)
	}
	blocks := w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, Prompt: p, Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolApproved, chat.ToolDenied}}))
	for _, b := range blocks {
		if b["type"] == "rich_text" && strings.Contains(argsOf(t, b), chat.Truncated) {
			t.Errorf("an ending of a prompt that fit cuts %.60q", argsOf(t, b))
		}
	}
}

// A press of each button of an approval, as Slack delivers it, arrives in the neutral sink as its action, with the prompt's
// ID, and the radios of a Submit as the verdicts the person chose.
func TestEachButtonOfAnApprovalIsDecodedToItsAction(t *testing.T) {
	w := newWorld(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.p.Run(ctx, rec) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); w.fake.Connections() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
	radio := func(v string) map[string]any {
		return map[string]any{"type": "radio_buttons", "selected_option": map[string]any{"value": v}}
	}

	w.post(t, chat.Ask(approvalOf(manyTools(1)...)))
	one, _ := w.last(t)
	w.post(t, chat.Ask(approvalOf(manyTools(3)...)))
	several, _ := w.last(t)
	steps := []struct {
		name string
		do   func() error
		want chat.Interaction
		at   fakeslack.Message
	}{
		{"Approve of one tool", func() error { _, err := w.fake.Click("U1", one.TS, "approve_all", approvalID); return err }, chat.Interaction{Action: chat.ActionApproveAll}, one},
		{"Deny of one tool", func() error { _, err := w.fake.Click("U1", one.TS, "deny_all", approvalID); return err }, chat.Interaction{Action: chat.ActionDenyAll}, one},
		{"Approve all", func() error { _, err := w.fake.Click("U1", several.TS, "approve_all", approvalID); return err }, chat.Interaction{Action: chat.ActionApproveAll}, several},
		{"Deny all", func() error { _, err := w.fake.Click("U1", several.TS, "deny_all", approvalID); return err }, chat.Interaction{Action: chat.ActionDenyAll}, several},
		{"Submit with two of three radios set", func() error {
			_, err := w.fake.Submit("U1", several.TS, "submit", fakeslack.StateValues{"q0": {"r0": radio("0")}, "q2": {"r2": radio("1")}})
			return err
		}, chat.Interaction{Answers: []chat.Answer{{Question: 0, Values: []string{"0"}}, {Question: 2, Values: []string{"1"}}}}, several},
		{"Submit with none set", func() error { _, err := w.fake.Submit("U1", several.TS, "submit", nil); return err }, chat.Interaction{}, several},
	}
	for n, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		rec.wait(t, st.name, func() bool { return len(rec.presses) > n })
		got := rec.presses[n]
		if got.User != "U1" || got.PromptID != approvalID || got.At.ID != st.at.TS || got.Action != st.want.Action || len(got.Answers) != len(st.want.Answers) {
			t.Errorf("%s: press = %+v, want the action %v, the prompt's ID and %d answers", st.name, got, st.want.Action, len(st.want.Answers))
			continue
		}
		for i, a := range got.Answers {
			if a.Question != st.want.Answers[i].Question || strings.Join(a.Values, ",") != strings.Join(st.want.Answers[i].Values, ",") {
				t.Errorf("%s: answer %d = %+v, want %+v", st.name, i, a, st.want.Answers[i])
			}
			if _, ok := a.ToolVerdict(); !ok {
				t.Errorf("%s: answer %d = %+v is no verdict", st.name, i, a)
			}
		}
	}
}

// A prompt of 24 tools fits in 50 blocks. Its ending has the status block too, and so room for one line of the two (the
// sub-agent's and the hint) and not both: what has no room is left out, and every tool is kept, with its arguments.
func TestTheEndingOfTwentyFourToolsLeavesOutTheLinesItHasNoRoomFor(t *testing.T) {
	w := newWorld(t)
	p := approvalOf(manyTools(24)...)
	p.Subagent, p.Hint = "sre", "Clean up?"
	if err := w.p.Fits(p); err != nil {
		t.Fatal(err)
	}
	verdicts := make([]chat.ToolVerdict, 24)
	for i := range verdicts {
		verdicts[i] = chat.ToolApproved
	}
	blocks := w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, Prompt: p, Verdicts: verdicts}))
	if len(blocks) != 50 || textOfContext(blocks[1]) != "Raised by sub-agent sre" || blocks[2]["type"] != "section" {
		t.Fatalf("%d blocks, the second %v and the third %v; want 50: the status, the sub-agent's line and 24 tools, and no hint", len(blocks), blocks[1]["type"], blocks[2]["type"])
	}
	if got := argsOf(t, blocks[len(blocks)-1]); got != `{"n":23}` {
		t.Errorf("the last tool's arguments are %q: the ending cut what was decided", got)
	}
	p.Tools = p.Tools[:23]
	blocks = w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, Prompt: p, Verdicts: verdicts[:23]}))
	if len(blocks) != 49 || textOfContext(blocks[1]) != "Raised by sub-agent sre" || textOfContext(blocks[2]) != "Clean up?" {
		t.Errorf("%d blocks; want 49 with both lines for 23 tools", len(blocks))
	}
}

func textOfContext(b map[string]any) string {
	els := asList(b["elements"])
	if len(els) == 0 {
		return ""
	}
	s, _ := els[0].(map[string]any)["text"].(string)
	return s
}

// For each tool of several, the first text of the block that holds its arguments and the label of its input are the same
// numbered name, so that a reader pairs the arguments with the radio under them.
func TestTheNameOfAToolIsTheSameInItsBlockAndItsLabel(t *testing.T) {
	w := newWorld(t)
	tools := []chat.Tool{{Name: "kubectl & <b>", Args: "{}"}, {Name: "", Args: "x"}, {Name: "ping", Args: ""}, {Name: "a\u200bb", Args: "y"}}
	blocks := w.post(t, chat.Ask(approvalOf(tools...)))
	for i := range tools {
		name, _, _ := richParts(t, blocks[1+2*i])
		label := labelOf(blocks[2+2*i])
		if label == "" || unescaped.Replace(label) != name {
			t.Errorf("tool %d: the block says %q and the label %q", i, name, label)
		}
	}
	if name, _, _ := richParts(t, blocks[1]); name != "1. kubectl & <b>" {
		t.Errorf("the name in the block is %q, want it numbered and not escaped", name)
	}
	if name, _, _ := richParts(t, blocks[7]); name != "4. ab" {
		t.Errorf("the name in the block is %q, want it cleaned", name)
	}
}

// A press of a button that is not one of the prompt's, in the blocks of a prompt, names no prompt and so records none.
func TestAnUnknownButtonOfAPromptNamesNoPrompt(t *testing.T) {
	w := newWorld(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.p.Run(ctx, rec) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); w.fake.Connections() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
	// The fake can only press a button that is on a message, so the message is posted with a button that the adapter does not render.
	blocks := `[{"type":"actions","block_id":"submit","elements":[{"type":"button","action_id":"approve_every_tool","value":"` + approvalID + `","text":{"type":"plain_text","text":"x"}}]}]`
	ts, err := w.c.Post(t.Context(), "C1", w.target.Thread, slack.Content{Text: "x", Blocks: []byte(blocks)}, "tag-unknown")
	if err != nil {
		t.Fatal(err)
	}
	m := fakeslack.Message{TS: ts}
	if _, err := w.fake.Click("U1", m.TS, "approve_every_tool", approvalID); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "the press", func() bool { return len(rec.presses) > 0 })
	if got := rec.presses[0]; got.PromptID != "" || got.Action != chat.ActionSubmit || len(got.Answers) != 0 {
		t.Errorf("press = %+v, want one that names no prompt and carries nothing", got)
	}
}
