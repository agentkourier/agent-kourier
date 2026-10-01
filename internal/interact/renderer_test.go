package interact_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const hostile = "<!channel> <@U123> <https://x|y> & end"

func askUserOf(questions ...dialect.Question) dialect.Interaction {
	return dialect.Interaction{Kind: dialect.AskUser, TaskID: "t1", Hint: hostile, RequestID: "r1", Questions: questions}
}

func blocksOf(t *testing.T, c slack.Content) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal(c.Blocks, &blocks); err != nil {
		t.Fatalf("blocks %s: %v", c.Blocks, err)
	}
	return blocks
}

// flat is the blocks of c as plain JSON text with nothing escaped for HTML, so that a test looking for
// "<!channel>" finds it where a person would read it. Its raw bytes spell "<" as \u003c.
func flat(t *testing.T, c slack.Content) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(c.Blocks, &decoded); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(decoded); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// slackOver returns courier's own client over a fake Slack with a thread in C1, and its root.
func slackOver(t *testing.T) (*fakeslack.Server, *slack.Client, string) {
	t.Helper()
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	_, root, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("root", false))
	if err != nil {
		t.Fatal(err)
	}
	return fake, slack.New(slack.Config{BotToken: secret.New("xoxb-fake"), AppToken: secret.New("xapp-fake"), APIURL: fake.URL() + "/"}), root
}

var askBy = time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

// TestPromptContentIsAcceptedBySlack posts the prompt through courier's client to the fake, which
// refuses a block type it does not know, and then edits it into the replacement. Every kind of control
// is in it: free text, one choice, several, a select, and the numbered fallback.
func TestPromptContentIsAcceptedBySlack(t *testing.T) {
	fake, client, root := slackOver(t)
	var r interact.Renderer
	in := askUserOf(
		dialect.Question{Text: "Why?"},
		dialect.Question{Text: "Which?", Choices: []string{"a", "b"}},
		dialect.Question{Text: "Some?", Choices: []string{"a", "b"}, Multiple: true},
		dialect.Question{Text: "Many?", Choices: choicesOf(30)},
		dialect.Question{Text: "Long?", Choices: []string{strings.Repeat("x", 200)}},
	)
	c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: in, Deadline: askBy})
	if err != nil {
		t.Fatalf("Prompt = %v", err)
	}
	ts, err := client.Post(t.Context(), "C1", root, c, "ix-1/reply")
	if err != nil {
		t.Fatalf("the fake refused the prompt: %v", err)
	}
	blocks := blocksOf(t, c)
	last := blocks[len(blocks)-1]
	if last["type"] != "actions" || last["block_id"] != render.SubmitBlockID || blocks[0]["type"] != "header" {
		t.Fatalf("blocks %v; want a header first and the Submit button last", blocks)
	}
	if button := last["elements"].([]any)[0].(map[string]any); button["value"] != "ix-1" {
		t.Fatalf("Submit button %v; want it valued with the interaction ID", button)
	}
	replacement, err := r.TimedOut(interact.Replacement{ID: "ix-1", Interaction: in, Deadline: askBy})
	if err != nil {
		t.Fatalf("TimedOut = %v", err)
	}
	if err := client.Update(t.Context(), "C1", ts, replacement); err != nil {
		t.Fatalf("the fake refused the replacement: %v", err)
	}
	for _, m := range fake.Thread("C1", root) {
		if m.TS == ts && (m.Text != "Timed out" || strings.Contains(string(m.Blocks), `"input"`) || strings.Contains(string(m.Blocks), `"button"`)) {
			t.Fatalf("edited prompt = %q %s; want the timed-out text with no input or button", m.Text, m.Blocks)
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func choicesOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("choice %d", i)
	}
	return out
}

// TestPromptTextIsInert: the text that stands for the message (a notification, a screen reader) is
// courier's own, and carries nothing the agent wrote; the blocks hold the hostile text escaped.
func TestPromptTextIsInert(t *testing.T) {
	var r interact.Renderer
	in := askUserOf(dialect.Question{Text: hostile, Choices: []string{hostile, "b"}})
	c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: in, Deadline: askBy})
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != "The agent needs your input. Answer by 2026-10-01 12:30 UTC." {
		t.Fatalf("text = %q; want courier's fixed words and the deadline", c.Text)
	}
	if got := flat(t, c); strings.Contains(got, "<!channel>") || strings.Contains(got, "<@U123>") || strings.Contains(got, "<https://x|y>") || !strings.Contains(got, "&lt;!channel&gt;") {
		t.Fatalf("blocks %s hold unescaped agent text, or none escaped", got)
	}
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: in}); err != nil || c.Text != "The agent needs your input." {
		t.Fatalf("with no deadline: text %q, %v; want no deadline in it", c.Text, err)
	}
}

// TestPromptShowsItsDeadlineBeforeSubmit: a context block, from the deadline alone, sits just above
// the Submit button; with no deadline there is none; and a prompt that fills the message to its
// limit goes without it rather than fail.
func TestPromptShowsItsDeadlineBeforeSubmit(t *testing.T) {
	var r interact.Renderer
	qs := func(n int) []dialect.Question { return make([]dialect.Question, n) }
	c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf(qs(2)...), Deadline: askBy})
	if err != nil {
		t.Fatal(err)
	}
	blocks := blocksOf(t, c)
	hint := flat(t, slack.Content{Blocks: mustJSON(t, blocks[len(blocks)-2:len(blocks)-1])})
	if len(blocks) != 5 || blocks[len(blocks)-2]["type"] != "context" || !strings.Contains(hint, "Answer by") || !strings.Contains(hint, "<!date^1790857800^") || !strings.Contains(hint, "2026-10-01 12:30 UTC") {
		t.Fatalf("blocks %v; want header, two inputs, the deadline hint and Submit", blocks)
	}
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf(qs(2)...)}); err != nil || len(blocksOf(t, c)) != 4 {
		t.Fatalf("with no deadline = %d blocks, %v; want 4, with no hint", len(blocksOf(t, c)), err)
	}
	// 48 questions fill the 50 blocks with the header and Submit: no room for the hint, no failure.
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf(qs(48)...), Deadline: askBy}); err != nil || len(blocksOf(t, c)) != 50 {
		t.Fatalf("a full prompt = %d blocks, %v; want 50 and no hint", len(blocksOf(t, c)), err)
	}
}

func TestPromptTooLargeAndUnusableKinds(t *testing.T) {
	var r interact.Renderer
	if _, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf(make([]dialect.Question, 49)...), Deadline: askBy}); !errors.Is(err, render.ErrPromptTooLarge) {
		t.Errorf("a prompt of 51 blocks = %v; want ErrPromptTooLarge", err)
	}
	// Even with a question in it, which would render, a tool approval is not an ask-user prompt.
	tool := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "t1", RequestID: "r1", Questions: []dialect.Question{{Text: "q"}}, Tools: []dialect.Tool{{ID: "c1", Name: "rm"}}}
	if _, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: tool}); err == nil {
		t.Error("a tool approval was rendered as a prompt")
	}
	if _, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf()}); err == nil {
		t.Error("a prompt with no question was rendered")
	}
	if _, err := r.Prompt(interact.Prompt{Interaction: askUserOf(dialect.Question{Text: "q"})}); err == nil {
		t.Error("a prompt with no ID was rendered")
	}
}

// TestTimedOutReplacementHoldsNoControlsAndNoAnswers: what the agent wrote is escaped, the deadline
// is when it ended, no input, button or answer is in it (a free-text answer is never shown, and a
// timed-out prompt has none), and a request that could not be read still has a replacement.
func TestTimedOutReplacementHoldsNoControlsAndNoAnswers(t *testing.T) {
	var r interact.Renderer
	in := askUserOf(dialect.Question{Text: "Why?"}, dialect.Question{Text: "Which?", Choices: []string{"a", "b"}})
	in.Hint, in.Nested = "closing hint", &dialect.Nested{Subagent: hostile}
	c, err := r.TimedOut(interact.Replacement{ID: "ix-1", Interaction: in, Deadline: askBy})
	if err != nil || c.Text != "Timed out" {
		t.Fatalf("TimedOut = %+v, %v", c, err)
	}
	got := flat(t, c)
	for _, bad := range []string{`"input"`, `"button"`, `"actions"`, "(answered)", "(no answer)", "<!channel>", "<@U123>", "<https://x|y>"} {
		if strings.Contains(got, bad) {
			t.Errorf("replacement %s holds %q", got, bad)
		}
	}
	if !strings.Contains(got, "<!date^1790857800^") || !strings.Contains(got, "Timed out") || !strings.Contains(got, "closing hint") || !strings.Contains(got, "&lt;!channel&gt;") {
		t.Errorf("replacement %s; want it dated at the deadline, with the hint and the escaped sub-agent", got)
	}
	if c, err := r.TimedOut(interact.Replacement{ID: "ix-1", Deadline: askBy}); err != nil || c.Text != "Timed out" || len(blocksOf(t, c)) == 0 {
		t.Errorf("TimedOut of an unreadable request = %+v, %v; want a replacement all the same", c, err)
	}
}

var (
	_ interact.PromptRenderer      = interact.Renderer{}
	_ interact.ReplacementRenderer = interact.Renderer{}
)

// TestNestedPromptNamesItsSubagent: a pause that a sub-agent raised says so, in a line under the header
// (as the replacement does), escaped as every word of the agent's is; a pause that no sub-agent raised
// has no such line; and a prompt with no room for the line goes without it rather than fail.
func TestNestedPromptNamesItsSubagent(t *testing.T) {
	var r interact.Renderer
	line := func(c slack.Content) []map[string]any { return blocksOf(t, c)[1:2] }
	in := askUserOf(dialect.Question{Text: "Why?"})
	in.Nested = &dialect.Nested{Subagent: hostile + "\nlookalike"}
	c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: in})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(flat(t, slack.Content{Blocks: mustJSON(t, line(c))})) // the encoder ends its output with a newline
	if blocks := blocksOf(t, c); len(blocks) != 4 || blocks[1]["type"] != "context" || !strings.Contains(got, "Raised by sub-agent *&lt;!channel&gt; &lt;@U123&gt;") ||
		strings.Contains(got, "<!channel>") || strings.Contains(got, "\n") {
		t.Fatalf("blocks %v; want header, the escaped one-line sub-agent, the input and Submit", blocks)
	}
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: askUserOf(dialect.Question{Text: "Why?"})}); err != nil || len(blocksOf(t, c)) != 3 || strings.Contains(flat(t, c), "sub-agent") {
		t.Fatalf("a pause with no sub-agent = %s, %v; want no sub-agent line", flat(t, c), err)
	}
	full := askUserOf(make([]dialect.Question, 48)...)
	full.Nested = in.Nested
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: full}); err != nil || len(blocksOf(t, c)) != 50 {
		t.Fatalf("a full nested prompt = %d blocks, %v; want 50 and no sub-agent line", len(blocksOf(t, c)), err)
	}
	// With both lines, the sub-agent's is under the header and the deadline's above Submit.
	if c, err := r.Prompt(interact.Prompt{ID: "ix-1", Interaction: in, Deadline: askBy}); err != nil || len(blocksOf(t, c)) != 5 || blocksOf(t, c)[1]["type"] != "context" || blocksOf(t, c)[3]["type"] != "context" {
		t.Fatalf("a nested prompt with a deadline = %v, %v; want both context lines", blocksOf(t, c), err)
	}
}
