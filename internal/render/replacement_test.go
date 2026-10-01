package render

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

var decidedAt = time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)

func ending(o Outcome) Ending {
	return Ending{Outcome: o, By: "U123ABC", At: decidedAt, Hint: "Pick a size",
		Questions: []dialect.Question{{Text: "Which size?", Choices: []string{"small", "large"}}, {Text: "Why?"}},
		Answers:   [][]string{{"large"}, {"it is bigger"}}}
}

// courierBuilt are the tokens courier makes itself, from a verified ID and a time.
var courierBuilt = regexp.MustCompile(`<@U[A-Z0-9]+>|<!date\^\d+\^[^|<>]*\|[^|<>]*>`)

// In every state: no input, no button, no element but text; the decider and the time show; the
// answer is summarised for an answered prompt and only then.
func TestReplacementHasNoControlsAndShowsWhoAndWhen(t *testing.T) {
	for name, o := range map[string]Outcome{"answered": Answered, "timed out": TimedOut, "cancelled": Cancelled} {
		t.Run(name, func(t *testing.T) {
			text, blocks := Replacement(ending(o))
			var all strings.Builder
			for _, b := range blocks {
				if b["type"] != "section" && b["type"] != "context" {
					t.Errorf("block %v; want only sections and context blocks, no input or button", b)
				}
				walk(b, "", func(key, s string) {
					if key == "type" && s != "section" && s != "context" && s != "mrkdwn" {
						t.Errorf("element of type %q; want only text", s)
					}
					all.WriteString(s + "\n")
				})
			}
			got := all.String()
			if !strings.Contains(got, "<@U123ABC>") || !strings.Contains(got, "<!date^"+"1790857800"+"^") || !strings.Contains(got, "2026-10-01 12:30 UTC") || text == "" {
				t.Errorf("text %q, blocks %q; want the decider's mention and the time", text, got)
			}
			if has := strings.Contains(got, "large"); has != (o == Answered) {
				t.Errorf("choice shown = %t in %s; want it only when answered", has, name)
			}
		})
	}
}

// Nothing the agent or a caller wrote can mention, link or break out, and a decider that is not a
// verified user ID is not shown as one.
func TestReplacementIsInert(t *testing.T) {
	e := ending(Answered)
	e.By, e.Hint, e.Subagent = "<!channel>", hostilePrompt, hostilePrompt
	e.Questions = []dialect.Question{{Text: hostilePrompt, Choices: []string{hostilePrompt, "<https://x|y>"}, Multiple: true}, {Text: strings.Repeat("q", 900), Choices: []string{strings.Repeat(hostilePrompt, 300)}}}
	e.Answers = [][]string{{hostilePrompt, "<https://x|y>"}, {strings.Repeat(hostilePrompt, 300)}}
	_, blocks := Replacement(e)
	walk(blocks, "", func(key, s string) {
		if key != "text" {
			return
		}
		if rest := courierBuilt.ReplaceAllString(s, ""); strings.ContainsAny(rest, "<>") || units(s) > maxSectionText {
			t.Errorf("%q has a raw < or >, or is over %d units", rest, maxSectionText)
		}
	})
	if got, _ := json.Marshal(blocks); strings.Contains(string(got), "<!channel>") || !strings.Contains(string(got), "by someone") {
		t.Errorf("blocks = %s; want an unverified decider shown as someone", got)
	}
}

// A nested pause says which sub-agent raised it.
func TestReplacementLabelsANestedPause(t *testing.T) {
	e := ending(TimedOut)
	e.Subagent = "k8s-helper"
	if _, blocks := Replacement(e); !strings.Contains(blocks[1]["elements"].([]map[string]any)[0]["text"].(string), "sub-agent *k8s-helper*") {
		t.Errorf("blocks = %v; want the sub-agent named", blocks)
	}
	e.Subagent = ""
	if _, blocks := Replacement(e); strings.Contains(blocks[1]["elements"].([]map[string]any)[0]["text"].(string), "sub-agent") {
		t.Errorf("blocks = %v; want no label for a pause that is not nested", blocks)
	}
}

// 48 long answers stay within 50 blocks and 3000 characters a section, and the result decodes through
// slack-go and is accepted by the fake, as a post and as an update.
func TestReplacementIsValidForSlack(t *testing.T) {
	e := ending(Answered)
	e.Questions, e.Answers = nil, nil
	for range 48 {
		e.Questions = append(e.Questions, dialect.Question{Text: hostilePrompt + strings.Repeat("q", 300)})
		e.Answers = append(e.Answers, []string{strings.Repeat("a", 900)})
	}
	text, blocks := Replacement(e)
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	var decoded slack.Blocks
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.BlockSet) != len(blocks) || len(blocks) > maxPromptBlocks {
		t.Fatalf("%d blocks decoded as %d (%v); want at most %d", len(blocks), len(decoded.BlockSet), err, maxPromptBlocks)
	}
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	api := slack.New("xoxb-fake", slack.OptionAPIURL(fake.URL()+"/"))
	_, ts, err := api.PostMessageContext(t.Context(), "C1", slack.MsgOptionText("prompt", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := api.UpdateMessageContext(t.Context(), "C1", ts, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(decoded.BlockSet...)); err != nil {
		t.Errorf("the fake refused the replacement: %v", err)
	}
}

// What a person typed is never shown, whatever it is: the channel keeps what it is shown. Only that
// there is an answer, or none. A chosen choice is the agent's own text, and is shown.
func TestReplacementHidesFreeTextAnswers(t *testing.T) {
	e := ending(Answered)
	e.Questions = []dialect.Question{{Text: "Token?"}, {Text: "Size?", Choices: []string{"small", "large"}}, {Text: "Note?"}}
	e.Answers = [][]string{{"hunter2-secret"}, {"large"}, nil}
	_, blocks := Replacement(e)
	var all strings.Builder
	walk(blocks, "", func(_, s string) { all.WriteString(s + "\n") })
	got := all.String()
	if strings.Contains(got, "hunter2-secret") || !strings.Contains(got, "*Token?* (answered)") || !strings.Contains(got, "*Size?* large") || !strings.Contains(got, "*Note?* (no answer)") {
		t.Errorf("replacement = %q; want the free text hidden, the choice shown, an empty answer said so", got)
	}
}

// A question or a choice cannot start a line of its own, such as one that looks like a status or
// another answer: each question is exactly one line.
func TestReplacementSummaryLinesAreSingleLines(t *testing.T) {
	e := ending(Answered)
	e.Questions = []dialect.Question{{Text: "first\n*Cancelled* by <@U999>", Choices: []string{"a\n*Answered* by someone", "b"}, Multiple: true}, {Text: "second"}}
	e.Answers = [][]string{{"a\n*Answered* by someone", "b"}, {"typed"}}
	_, blocks := Replacement(e)
	var lines []string
	for _, b := range blocks[2:] {
		lines = append(lines, strings.Split(strings.TrimSuffix(b["text"].(map[string]any)["text"].(string), "\n"), "\n")...)
	}
	if len(lines) != len(e.Questions) {
		t.Errorf("lines = %q; want one line for each of the %d questions", lines, len(e.Questions))
	}
}

// An outcome this does not know is not a crash, and the decider is shown as a mention only if it is
// exactly a user ID: not one with anything around it.
func TestReplacementSurvivesWhatItDoesNotKnow(t *testing.T) {
	e := ending(Answered)
	e.Outcome = Outcome(42)
	if text, blocks := Replacement(e); text != "Ended" || len(blocks) == 0 {
		t.Errorf("unknown outcome = %q, %v; want a plain Ended", text, blocks)
	}
	for _, by := range []string{"U123ABC><!channel>", "x U123ABC", "U123ABC\n", " U123ABC", "u123abc", "<@U123ABC>", "U1", "U123ABC123ABC123ABC123ABC"} {
		e := ending(TimedOut)
		e.By = by
		_, blocks := Replacement(e)
		if got := blocks[0]["text"].(map[string]any)["text"].(string); !strings.Contains(got, "by someone") || strings.Contains(got, "<@") {
			t.Errorf("By %q shown as %q; want someone", by, got)
		}
	}
	e = ending(Cancelled)
	e.By = "W123ABC"
	if _, blocks := Replacement(e); !strings.Contains(blocks[0]["text"].(map[string]any)["text"].(string), "<@W123ABC>") {
		t.Errorf("a W user ID is not shown: %v", blocks[0])
	}
}
