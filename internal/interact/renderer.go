package interact

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// promptText is the text that stands for a prompt where its blocks are not shown (a notification, a
// screen reader). It is courier's own words: nothing the agent wrote is in it.
const promptText = "The agent needs your input."

// maxBlocks is Slack's limit on the blocks of a message (UNCONFIRMED(WP-04): documented, not measured).
const maxBlocks = 50

// Renderer renders the messages of interactions, over package render: the prompt that asks, and the
// replacement that takes its place once it timed out. It lives here and not in render because it
// speaks this package's types, and render cannot import them. It holds no state.
type Renderer struct{}

// Prompt renders an ask-user prompt: a header from the agent's hint, an input for each question, a
// line with the deadline, and the Submit button valued with the interaction's ID (render.PromptBlocks).
// Everything the agent wrote is escaped there. A prompt that does not fit in one message is
// render.ErrPromptTooLarge, which no retry mends. The deadline line is left out of a prompt that has
// no room for it, rather than refusing a prompt that would otherwise fit.
func (Renderer) Prompt(p Prompt) (slack.Content, error) {
	if p.Interaction.Kind != dialect.AskUser {
		return slack.Content{}, fmt.Errorf("interact: no prompt is rendered for a %q pause", p.Interaction.Kind)
	}
	blocks, _, err := render.PromptBlocks(p.ID, p.Interaction.Hint, p.Interaction.Questions)
	if err != nil {
		return slack.Content{}, fmt.Errorf("interact: render the prompt: %w", err)
	}
	text := promptText
	if !p.Deadline.IsZero() {
		text += " Answer by " + utc(p.Deadline) + "."
		if len(blocks) < maxBlocks { // above the Submit button, which stays last
			hint := deadlineHint(p.Deadline)
			blocks = append(blocks[:len(blocks)-1:len(blocks)-1], hint, blocks[len(blocks)-1])
		}
	}
	return content(text, blocks)
}

// TimedOut renders the replacement of a prompt that timed out: the same as a decided one but with
// no answer, since there is none, at the time of the deadline, and no input or button (render.Replacement).
// A request that could not be read, which Replacement.Interaction then leaves zero, still has one.
func (Renderer) TimedOut(r Replacement) (slack.Content, error) {
	e := render.Ending{Outcome: render.TimedOut, At: r.Deadline, Hint: r.Interaction.Hint, Questions: r.Interaction.Questions}
	if r.Interaction.Nested != nil {
		e.Subagent = r.Interaction.Nested.Subagent
	}
	text, blocks := render.Replacement(e)
	return content(text, blocks)
}

// deadlineHint is the context block that says when the question closes, from a time alone: Slack
// shows the date in the reader's zone, and the fallback after the bar is in UTC.
func deadlineHint(t time.Time) map[string]any {
	text := fmt.Sprintf("Answer by <!date^%d^{date_short_pretty} at {time}|%s>", t.Unix(), utc(t))
	return map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": text}}}
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// content is text and blocks as the message courier sends.
func content(text string, blocks []map[string]any) (slack.Content, error) {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return slack.Content{}, fmt.Errorf("interact: encode the blocks: %w", err)
	}
	return slack.Content{Text: text, Blocks: raw}, nil
}
