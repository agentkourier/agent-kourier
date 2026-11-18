package teams

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
)

func mustRender(t *testing.T, o chat.Outgoing) outActivity {
	t.Helper()
	a, err := render(o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// No text of the agent's, a person's or an alert's can make a mention: "<at>" is never sent raw from it, in any place, so the only
// "<at>" of an activity is the one of the mention entity that Agent Kourier adds, and Teams cannot bind that entity to the agent's
// text instead.
func TestAtTagsInTextAreNeverSentRaw(t *testing.T) {
	t.Parallel()
	const tag = "<at>someone</at>"
	hostile := []string{tag, "<at>" + testUser + "</at>", "<at id=\"0\">x</at>", "`<at>someone</at>`", "```\n<at>someone</at>\n```",
		"    <at>someone</at>", "&lt;at&gt;someone&lt;/at&gt;", "<<at>>someone</at>"}
	for _, h := range hostile {
		for name, o := range map[string]chat.Outgoing{
			"body":         chat.Say(chat.Message{Parts: []chat.Part{chat.Body{Markdown: h}}}),
			"header":       chat.Say(chat.Message{Parts: []chat.Part{chat.Header{Text: h}}}),
			"note":         chat.Say(chat.Message{Parts: []chat.Part{chat.Note{Text: h}}}),
			"fallback":     chat.Say(chat.Message{Fallback: h}),
			"mention text": chat.Say(chat.Message{Parts: []chat.Part{chat.Mention{User: testUser, Text: h}, chat.Body{Markdown: h}}}),
			"forged user":  chat.Say(chat.Message{Parts: []chat.Part{chat.Mention{User: h, Text: "answered."}}}),
			"question":     chat.Ask(chat.Prompt{ID: "p", Questions: []chat.Question{{Text: h, Choices: []string{h}}}}),
			"ending":       chat.End(chat.Ending{Outcome: chat.Answered, By: h, Prompt: chat.Prompt{ID: "p", Hint: h}}),
		} {
			a := mustRender(t, o)
			raw := strings.Count(a.Text, "<at>")
			if raw != len(a.Entities) || strings.Contains(a.Text, "<at id") || strings.Contains(a.Text, "<<") {
				t.Errorf("%s of %q: %d raw <at> for %d entities in %q", name, h, raw, len(a.Entities), a.Text)
			}
			for _, e := range a.Entities {
				if e.Mentioned.ID != testUser || e.Text != tag || !strings.HasPrefix(a.Text, tag) {
					t.Errorf("%s of %q: the entity %+v in %q", name, h, e, a.Text)
				}
			}
		}
	}
}

// A Mention of a Microsoft Entra object ID is a mention entity of it, beside its text; of anything else, the word "someone", and no
// entity.
func TestMentions(t *testing.T) {
	t.Parallel()
	a := mustRender(t, chat.Say(chat.Message{Parts: []chat.Part{chat.Mention{User: testUser, Text: "answered *first*."}}}))
	if a.Text != `<at>someone</at> answered \*first\*.` || len(a.Entities) != 1 ||
		a.Entities[0] != (mentionEntity{Type: "mention", Text: "<at>someone</at>", Mentioned: mentioned{ID: testUser, Name: "someone"}}) {
		t.Errorf("a mention: %+v", a)
	}
	for _, forged := range []string{"U0123456789", "00000000-0000-4000-8000-00000000000A", testUser + " ", "28:" + testAppID, ""} {
		a := mustRender(t, chat.Say(chat.Message{Parts: []chat.Part{chat.Mention{User: forged, Text: "answered."}}}))
		if len(a.Entities) != 0 || a.Text != "someone answered." {
			t.Errorf("a mention of %q: %+v", forged, a)
		}
	}
}

// What escapeLine sends is pinned: a reader's view alone cannot tell an escape that works from one that a reader ignores.
func TestEscapeLine(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain words":                    "plain words",
		"<b>&amp;</b>":                   "&lt;b&gt;&amp;amp;&lt;/b&gt;",
		"[x](https://evil.test)":         "&#91;x&#93;(https://evil.test)",
		"*b* _i_ ~s~ `c` a|b \\":         `\*b\* \_i\_ \~s\~ \` + "`c\\`" + ` a\|b \\`,
		"# h":                            `\# h`,
		"- item":                         `\- item`,
		"+ item":                         `\+ item`,
		"===":                            `\===`,
		"12. item":                       `12\. item`,
		"3) item":                        `3\) item`,
		"a - b # c 1. d":                 "a - b # c 1. d",
		"   > quote":                     "&gt; quote",
		"see https://example.test/a now": "see https://example.test/a now",
		"<https://evil.test/login>":      "&lt;&#104;ttps://evil.test/login&gt;",
		"go to www.evil.test/a_b":        `go to &#119;ww.evil.test/a\_b`,
	} {
		if got := escapeLine(in); got != want {
			t.Errorf("escapeLine(%q) = %q, want %q", in, got, want)
		}
		shown := chattest.ScanCommonMark(escapeLine(in))
		for _, a := range shown.Active {
			if a.Kind != chattest.KindAutolink {
				t.Errorf("escapeLine(%q) is read as a %s", in, a.Kind)
			}
		}
		if wantShown := strings.TrimLeft(in, " "); shown.Displayed != wantShown {
			t.Errorf("escapeLine(%q) is shown as %q", in, shown.Displayed)
		}
	}
}

// The writer of the Body sends no "<" raw, in prose, in a span or in a block, and keeps the agent's code as code where it can.
func TestMarkdownSendsNoAngleBracket(t *testing.T) {
	t.Parallel()
	for md, want := range map[string]string{
		"Run `kubectl get pods` now.":                       "Run `kubectl get pods` now.",
		"Run `kubectl get pods -l app=<name>` now.":         "Run \\`kubectl get pods -l app=&lt;name>\\` now.",
		"```\nweb-0   <none>\n```":                          "```\nweb-0   &lt;none>\n```",
		"a & b <c> [d](e)":                                  "a &amp; b &lt;c> \\[d\\](e)",
		"**bold** and _it_":                                 "**bold** and _it_",
		"    <b>indented</b>":                               "```\n&lt;b>indented&lt;/b>\n```",
		"- item <at>someone</at>\n- `<at>someone</at>` too": "- item &lt;at>someone&lt;/at>\n- \\`&lt;at>someone&lt;/at>\\` too",
	} {
		got := markdown(md, math.MaxInt)
		if got != want {
			t.Errorf("markdown(%q) = %q, want %q", md, got, want)
		}
		if strings.Contains(got, "<") {
			t.Errorf("markdown(%q) sends a raw <: %q", md, got)
		}
	}
	if got := markdown(strings.Repeat("x", 100), 30); chat.Units(got) > 30 || !strings.HasSuffix(got, chat.Truncated) {
		t.Errorf("a cut body: %q", got)
	}
}

// A message is held to the budget: a body too long for it is cut further, and a prompt that cannot fit is ErrTooLarge.
func TestTheBudgetOfAMessage(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("中\\", maxBodyUnits) // three bytes and an escaped backslash a character: over the budget at full length
	a := mustRender(t, chat.Say(chat.Message{Parts: []chat.Part{chat.Header{Text: "h"}, chat.Body{Markdown: big}}}))
	n, err := size(a)
	if err != nil || n > maxMessageBytes || !strings.Contains(a.Text, chat.Truncated) {
		t.Errorf("a long body: %d bytes, %v", n, err)
	}
	var notes []chat.Part
	for range 40 {
		notes = append(notes, chat.Note{Text: strings.Repeat("中", maxTextUnits)})
	}
	if _, err := render(chat.Say(chat.Message{Parts: notes})); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("forty long notes: %v, want ErrTooLarge", err)
	}
	c := newClientTest(t).c
	if err := c.Fits(chat.Prompt{ID: "p", Questions: []chat.Question{{Text: "which?", Choices: []string{"a", "b"}}}}); err != nil {
		t.Errorf("a small prompt: %v", err)
	}
	var qs []chat.Question
	for range 40 {
		qs = append(qs, chat.Question{Text: strings.Repeat("q", maxTextUnits)})
	}
	if err := c.Fits(chat.Prompt{ID: "p", Questions: qs}); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("a large prompt: %v", err)
	}
	if err := c.Fits(chat.Prompt{ID: "p", Tools: []chat.Tool{{Name: "t"}}}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("a prompt of tools: %v", err)
	}
}

// A prompt is shown as text, with the line that says it cannot be answered here; its ending says who decided and what.
func TestPromptsAreText(t *testing.T) {
	t.Parallel()
	p := chat.Prompt{ID: "p", Hint: "Deploy", Questions: []chat.Question{{Text: "Where?", Choices: []string{"prod", "staging"}}}}
	a := mustRender(t, chat.Ask(p))
	want := "Deploy\n\nWhere?\n\nOption 1: prod\n\nOption 2: staging\n\n" + noAnswers
	if a.Text != want {
		t.Errorf("prompt text %q, want %q", a.Text, want)
	}
	e := mustRender(t, chat.End(chat.Ending{Outcome: chat.Answered, By: testUser, Prompt: p, Answers: [][]string{{"prod"}}}))
	if e.Text != "<at>someone</at> answered.\n\nDeploy\n\nWhere?\n\nAnswer: prod" || len(e.Entities) != 1 {
		t.Errorf("ending %+v", e)
	}
}
