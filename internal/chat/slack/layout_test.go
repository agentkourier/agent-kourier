package slack_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// world is a Slack adapter over the fake Slack, with a thread to post in.
type world struct {
	fake   *fakeslack.Server
	p      *slack.Platform
	c      *slack.Client
	target chat.Target
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	root, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "root"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	return &world{fake: fake, p: slack.NewPlatform(c, "agent-kourier-system/slack-main"), c: c, target: chat.Target{Conn: "agent-kourier-system/slack-main", Channel: "C1", Thread: root}}
}

// last is the newest message in the thread, and its blocks.
func (w *world) last(t *testing.T) (fakeslack.Message, []map[string]any) {
	t.Helper()
	th := w.fake.Thread("C1", w.target.Thread)
	m := th[len(th)-1]
	var blocks []map[string]any
	if len(m.Blocks) > 0 {
		if err := json.Unmarshal(m.Blocks, &blocks); err != nil {
			t.Fatal(err)
		}
	}
	return m, blocks
}

func types(blocks []map[string]any) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i], _ = b["type"].(string)
	}
	return out
}

func (w *world) post(t *testing.T, o chat.Outgoing) []map[string]any {
	t.Helper()
	if _, err := w.p.Post(t.Context(), w.target, o, fmt.Sprintf("tag-%d", time.Now().UnixNano())); err != nil {
		t.Fatal(err)
	}
	_, blocks := w.last(t)
	return blocks
}

// element returns the key of a block's element, which tests read as a map.
func element(b map[string]any) map[string]any { e, _ := b["element"].(map[string]any); return e }

func textOf(b map[string]any) string {
	t, _ := b["text"].(map[string]any)
	s, _ := t["text"].(string)
	return s
}

func TestPostRendersEachPartAsItsBlockAndTagsTheMessage(t *testing.T) {
	w := newWorld(t)
	msg := chat.Message{Fallback: "the fallback", Parts: []chat.Part{
		chat.Header{Text: "Title"}, chat.Body{Markdown: "*body* & <@U1> [x](y)"}, chat.Context{Text: "small"}, chat.Note{Text: "a note\non two lines"},
		chat.ToolLine{Name: "kubectl", Summary: "a=1, b=2"},
		chat.Buttons{Buttons: []chat.Button{{ID: "go", Label: "Go", Value: "v1", Primary: true}, {ID: "no", Label: "No", Value: "v2"}}},
	}}

	ref, err := w.p.Post(t.Context(), w.target, chat.Say(msg), "outbox-1")
	if err != nil {
		t.Fatal(err)
	}
	m, blocks := w.last(t)

	if got := types(blocks); strings.Join(got, ",") != "header,markdown,context,section,context,actions" {
		t.Fatalf("blocks = %v, want header, markdown, context, section, context, actions in order", got)
	}
	if m.Text != "the fallback" || textOf(blocks[0]) != "Title" || blocks[1]["text"] != `*body* &amp; &lt;@U1&gt; \[x\](y)` {
		t.Errorf("fallback %q, header %q, markdown %q; want the fallback, the title, and the markdown with & < > as entities and [ ] escaped", m.Text, textOf(blocks[0]), blocks[1]["text"])
	}
	if textOf(blocks[3]) != "a note\non two lines" {
		t.Errorf("note = %q, want both lines (a note may run to several)", textOf(blocks[3]))
	}
	tool, _ := blocks[4]["elements"].([]any)
	if line := tool[0].(map[string]any)["text"]; line != "Calling kubectl(a=1, b=2)" {
		t.Errorf("tool line = %q, want the name and the summary", line)
	}
	btns := blocks[5]["elements"].([]any)
	first, second := btns[0].(map[string]any), btns[1].(map[string]any)
	if blocks[5]["block_id"] != "buttons5" {
		t.Errorf("block_id = %v; want one of the part's own (its place in the message), which a press names and Slack would otherwise make up", blocks[5]["block_id"])
	}
	if first["action_id"] != "go" || first["value"] != "v1" || first["style"] != "primary" || second["style"] != nil {
		t.Errorf("buttons = %v %v; want the IDs and values, and primary only on the first", first, second)
	}
	if m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "outbox-1" || ref.ID != m.TS || ref.Channel != "C1" || ref.Thread != w.target.Thread || ref.Platform != "slack" || ref.Conn != "agent-kourier-system/slack-main" {
		t.Errorf("metadata %+v, ref %+v; want the outbox tag, and a Ref of this message", m.Metadata, ref)
	}
	found, ok, err := w.p.Find(t.Context(), w.target, "outbox-1", time.Now().Add(-time.Hour))
	if err != nil || !ok || found.ID != ref.ID {
		t.Errorf("Find = %+v, %t, %v; want the message by its tag", found, ok, err)
	}
	if _, ok, _ := w.p.Find(t.Context(), w.target, "outbox-9", time.Now().Add(-time.Hour)); ok {
		t.Error("Find found a tag that was never posted")
	}
}

// A message that is only a fallback is shown as plain text, in a block of its own, so that what it says is not
// read as mrkdwn; and one with nothing to show says so, since Slack refuses an empty message.
func TestAMessageThatIsOnlyAFallbackIsShownAsPlainText(t *testing.T) {
	w := newWorld(t)
	blocks := w.post(t, chat.Say(chat.Message{Fallback: "*not bold* <!here>"}))
	if got := types(blocks); len(got) != 1 || got[0] != "section" || textOf(blocks[0]) != "*not bold* &lt;!here&gt;" {
		t.Errorf("blocks = %v %q; want one section of plain text with the entities", got, textOf(blocks[0]))
	}
	for _, s := range []string{"\u200b\n\u200c", ""} {
		blocks = w.post(t, chat.Say(chat.Message{Fallback: s, Parts: []chat.Part{chat.Header{Text: "\u200b"}}}))
		if textOf(blocks[0]) != "(empty message)" {
			t.Errorf("a message of %q showed %q, want the words that say it is empty", s, textOf(blocks[0]))
		}
	}
}

func TestAMessageWithNoFallbackTakesItsFirstTextForIt(t *testing.T) {
	w := newWorld(t)
	w.post(t, chat.Say(chat.Message{Parts: []chat.Part{chat.Context{Text: "small"}, chat.Header{Text: "Title"}}}))
	if m, _ := w.last(t); m.Text != "small" {
		t.Errorf("text = %q, want the first part's", m.Text)
	}
}

func TestAMessageOfMoreThanFiftyBlocksIsTooLarge(t *testing.T) {
	w := newWorld(t)
	var parts []chat.Part
	for range 51 {
		parts = append(parts, chat.Note{Text: "n"})
	}
	if _, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Parts: parts}), "t"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post = %v, want ErrTooLarge", err)
	}
}

func askOf(qs ...chat.Question) chat.Prompt {
	return chat.Prompt{ID: "ix-0123456789abcdef", Hint: "Which environment?", Questions: qs}
}

func choices(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("option %d", i)
	}
	return out
}

// Each question gets the control that fits it: text for free text, radio buttons or checkboxes for up to ten
// choices, a select for more, and, for choices that cannot be options, a numbered list and an input for the numbers.
func TestAPromptGivesEachQuestionTheControlThatFitsIt(t *testing.T) {
	w := newWorld(t)
	long := strings.Repeat("a long choice ", 8) // over 75 units
	p := askOf(
		chat.Question{Text: "free?"},
		chat.Question{Text: "one of three", Choices: choices(3)},
		chat.Question{Text: "some of three", Choices: choices(3), Multiple: true},
		chat.Question{Text: "one of twelve", Choices: choices(12)},
		chat.Question{Text: "some of twelve", Choices: choices(12), Multiple: true},
		chat.Question{Text: "too long to be options", Choices: []string{"a", long, "c"}},
		chat.Question{Text: "too many", Choices: choices(101)},
	)

	blocks := w.post(t, chat.Ask(p))

	byID := map[string]map[string]any{}
	for _, b := range blocks {
		if id, _ := b["block_id"].(string); id != "" {
			byID[id] = b
		}
	}
	for q, want := range map[string][2]string{
		"q0": {"plain_text_input", "t0"}, "q1": {"radio_buttons", "r1"}, "q2": {"checkboxes", "c2"}, "q3": {"static_select", "s3"},
		"q4": {"multi_static_select", "m4"}, "q5": {"plain_text_input", "n5"}, "q6": {"plain_text_input", "n6"},
	} {
		el := element(byID[q])
		if el["type"] != want[0] || el["action_id"] != want[1] {
			t.Errorf("block %s has the element %v %v, want %s %s", q, el["type"], el["action_id"], want[0], want[1])
		}
	}
	radio := element(byID["q1"])["options"].([]any)
	if len(radio) != 3 || radio[1].(map[string]any)["value"] != "1" || radio[1].(map[string]any)["text"].(map[string]any)["text"] != "option 1" {
		t.Errorf("radio options = %v, want three, valued by index and showing the choice", radio)
	}
	// The numbered list holds every choice, each in a field of its own and numbered, the long one cut visibly.
	var numbered []string
	for _, b := range blocks {
		fields, _ := b["fields"].([]any)
		for _, f := range fields {
			numbered = append(numbered, f.(map[string]any)["text"].(string))
		}
	}
	if len(numbered) != 3+101 || numbered[0] != "1. a" || !strings.HasSuffix(numbered[1], chat.Truncated) && !strings.HasPrefix(numbered[1], "2. a long choice") || numbered[3+100] != "101. option 100" {
		t.Errorf("%d fields in numbered lists, first %q, second %q, last %q; want every choice, numbered", len(numbered), numbered[0], numbered[1], numbered[len(numbered)-1])
	}
	for _, line := range numbered {
		if chat.Units(line) > 200 {
			t.Errorf("a list line is %d units, over the 200 of a choice's line", chat.Units(line))
		}
	}
	last := blocks[len(blocks)-1]
	btn := last["elements"].([]any)[0].(map[string]any)
	if last["block_id"] != "submit" || btn["action_id"] != "submit" || btn["value"] != p.ID || btn["style"] != "primary" {
		t.Errorf("last block = %v, want the Submit button valued with the prompt's ID", last)
	}
	if textOf(blocks[0]) != "Which environment?" {
		t.Errorf("header = %q, want the hint", textOf(blocks[0]))
	}
}

func TestAPromptSaysItNeedsInputAndWhenItCloses(t *testing.T) {
	w := newWorld(t)
	deadline := time.Date(2026, 10, 2, 15, 4, 0, 0, time.UTC)
	p := askOf(chat.Question{Text: "q"})
	p.Hint, p.Subagent, p.Deadline = "", "sre-agent", deadline

	blocks := w.post(t, chat.Ask(p))
	m, _ := w.last(t)

	if m.Text != "The agent needs your input. Answer by 2026-10-02 15:04 UTC." {
		t.Errorf("text = %q, want Agent Kourier's own words and the deadline, and nothing the agent wrote", m.Text)
	}
	if textOf(blocks[0]) != "The agent needs your input" {
		t.Errorf("header with no hint = %q, want the default", textOf(blocks[0]))
	}
	sub := blocks[1]["elements"].([]any)[0].(map[string]any)
	if blocks[1]["type"] != "context" || sub["text"] != "Raised by sub-agent sre-agent" {
		t.Errorf("block 1 = %v, want the sub-agent's line under the header", blocks[1])
	}
	hint := blocks[len(blocks)-2]["elements"].([]any)[0].(map[string]any)
	if hint["type"] != "mrkdwn" || !strings.Contains(hint["text"].(string), fmt.Sprintf("<!date^%d^", deadline.Unix())) || !strings.Contains(hint["text"].(string), "|2026-10-02 15:04 UTC>") {
		t.Errorf("deadline line = %v, want a date token built from the time, with UTC as its fallback", hint)
	}
}

// Fits is the 50-block rule: a header, a block for each question, and the Submit button. The sub-agent's line and
// the deadline's are left out of a prompt that has no room for them, and are not a reason to refuse it.
func TestFitsIsTheFiftyBlockRule(t *testing.T) {
	w := newWorld(t)
	questions := func(n int) []chat.Question {
		qs := make([]chat.Question, n)
		for i := range qs {
			qs[i] = chat.Question{Text: fmt.Sprintf("q%d", i)}
		}
		return qs
	}
	for _, tc := range []struct {
		name string
		n    int
		want error
	}{{"two blocks and a question each", 5, nil}, {"exactly fifty", 48, nil}, {"fifty-one", 49, chat.ErrTooLarge}, {"far too many", 500, chat.ErrTooLarge}} {
		if err := w.p.Fits(askOf(questions(tc.n)...)); !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("%s: Fits = %v, want %v", tc.name, err, tc.want)
		}
	}
	full := askOf(questions(48)...)
	full.Subagent, full.Deadline = "sre", time.Now().Add(time.Hour)
	blocks := w.post(t, chat.Ask(full))
	if len(blocks) != 50 || blocks[0]["type"] != "header" || blocks[1]["type"] != "input" {
		t.Errorf("a prompt of 50 blocks has %d, and %v after the header; want the optional lines left out", len(blocks), blocks[1]["type"])
	}
	roomy := askOf(questions(47)...)
	roomy.Subagent, roomy.Deadline = "sre", time.Now().Add(time.Hour)
	if got := w.post(t, chat.Ask(roomy)); len(got) != 50 || got[1]["type"] != "context" {
		t.Errorf("a prompt with room for both lines has %d blocks and %v after the header; want 50 and the sub-agent's line", len(got), got[1]["type"])
	}
	if err := w.p.Fits(chat.Prompt{ID: strings.Repeat("x", 2001), Questions: []chat.Question{{Text: "q"}}}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Fits of a prompt whose ID cannot be a button's value = %v, want ErrInvalid", err)
	}
	if err := w.p.Fits(chat.Prompt{ID: "ix"}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Fits of a prompt with no question = %v, want ErrInvalid", err)
	}
}

func TestAnEndingShowsHowItEndedAndWhoByAndWhatWasAnswered(t *testing.T) {
	w := newWorld(t)
	at := time.Date(2026, 10, 2, 15, 4, 0, 0, time.UTC)
	p := askOf(chat.Question{Text: "free?"}, chat.Question{Text: "pick", Choices: choices(3)}, chat.Question{Text: "skipped"})
	p.Subagent = "sre"

	blocks := w.post(t, chat.End(chat.Ending{Outcome: chat.Answered, By: "U0123456789", At: at, Prompt: p, Answers: [][]string{{"(answered)"}, {"option 1", "option 2"}}}))
	m, _ := w.last(t)

	status := blocks[0]["text"].(map[string]any)["text"].(string)
	if m.Text != "Answered" || !strings.HasPrefix(status, "*Answered* by <@U0123456789> <!date^") {
		t.Errorf("text %q, status %q; want it answered by the verified user, with the date", m.Text, status)
	}
	var fields []string
	for _, b := range blocks[1:] {
		for _, f := range asList(b["fields"]) {
			fields = append(fields, f.(map[string]any)["text"].(string))
		}
	}
	if strings.Join(fields, "|") != "free?: (answered)|pick: option 1, option 2|skipped: (no answer)" {
		t.Errorf("answer fields = %q, want a line for each question, one field each", fields)
	}
	for _, b := range blocks {
		if b["type"] == "actions" || b["type"] == "input" {
			t.Errorf("block %v is left in the ending: no control remains", b["type"])
		}
	}
}

func asList(v any) []any { l, _ := v.([]any); return l }

func TestAnEndingThatWasNotAnsweredShowsNoAnswersAndNamesNoOneItCannotVerify(t *testing.T) {
	w := newWorld(t)
	p := askOf(chat.Question{Text: "q"})
	for _, tc := range []struct {
		name string
		e    chat.Ending
		want string
	}{
		{"timed out", chat.Ending{Outcome: chat.TimedOut, Prompt: p}, "*Timed out*"},
		{"cancelled by someone who is no user ID", chat.Ending{Outcome: chat.Cancelled, By: "<!channel>", Prompt: p}, "*Cancelled* by someone"},
		{"cancelled by a user", chat.Ending{Outcome: chat.Cancelled, By: "W0123ABC", Prompt: p}, "*Cancelled* by <@W0123ABC>"},
	} {
		blocks := w.post(t, chat.End(tc.e))
		if status := blocks[0]["text"].(map[string]any)["text"].(string); status != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, status, tc.want)
		}
		for _, b := range blocks[1:] {
			if b["fields"] != nil {
				t.Errorf("%s: an ending that was not answered shows answers: %v", tc.name, b)
			}
		}
	}
}

// mrkdwnObjects are the mrkdwn text objects anywhere in blocks.
func mrkdwnObjects(v any) []map[string]any {
	var out []map[string]any
	switch v := v.(type) {
	case []map[string]any:
		for _, e := range v {
			out = append(out, mrkdwnObjects(e)...)
		}
	case []any:
		for _, e := range v {
			out = append(out, mrkdwnObjects(e)...)
		}
	case map[string]any:
		if v["type"] == "mrkdwn" {
			out = append(out, v)
		}
		for _, e := range v {
			out = append(out, mrkdwnObjects(e)...)
		}
	}
	return out
}

// TestEveryMrkdwnObjectIsVerbatim: with verbatim false, which is Slack's default, Slack links channel names
// and parses some mentions in a mrkdwn object, whatever the escaping did. Every mrkdwn object Agent Kourier sends
// says verbatim true, and none is sent without it. (UNCONFIRMED(WP-04): what Slack does with each value.)
func TestEveryMrkdwnObjectIsVerbatim(t *testing.T) {
	w := newWorld(t)
	at := time.Date(2026, 10, 2, 15, 4, 0, 0, time.UTC)
	p := askOf(chat.Question{Text: "q"})
	p.Subagent, p.Deadline = "sre-agent", at
	msg := chat.Message{Fallback: "f", Parts: []chat.Part{chat.Header{Text: "h"}, chat.Body{Markdown: "b"}, chat.Context{Text: "c"}, chat.Note{Text: "n"}, chat.ToolLine{Name: "t"}, chat.Mention{User: "U0123456789", Text: "said"}}}
	for name, tc := range map[string]struct {
		o    chat.Outgoing
		want int
	}{
		"a message, with a mention":   {chat.Say(msg), 1},
		"a prompt, with its deadline": {chat.Ask(p), 1},
		"an ending":                   {chat.End(chat.Ending{Outcome: chat.Answered, By: "U0123456789", At: at, Prompt: p, Answers: [][]string{{"(answered)"}}}), 1},
	} {
		objs := mrkdwnObjects(w.post(t, tc.o))
		if len(objs) < tc.want {
			t.Errorf("%s: %d mrkdwn objects, want at least %d (the test is not looking at them)", name, len(objs), tc.want)
		}
		for _, o := range objs {
			if o["verbatim"] != true {
				t.Errorf("%s: mrkdwn object %v has no verbatim true", name, o)
			}
		}
	}
}
