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
			if has := strings.Contains(got, "it is bigger"); has != (o == Answered) {
				t.Errorf("answer shown = %t in %s; want it only when answered", has, name)
			}
		})
	}
}

// Nothing the agent or a caller wrote can mention, link or break out, and a decider that is not a
// verified user ID is not shown as one.
func TestReplacementIsInert(t *testing.T) {
	e := ending(Answered)
	e.By, e.Hint, e.Subagent = "<!channel>", hostilePrompt, hostilePrompt
	e.Questions = []dialect.Question{{Text: hostilePrompt}, {Text: strings.Repeat("q", 900)}}
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
