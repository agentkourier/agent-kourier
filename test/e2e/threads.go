package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	chatslack "github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// BotMessages returns the messages of a thread that the bot posted, in the order the thread has them.
func BotMessages(thread []fakeslack.MessageView) []fakeslack.MessageView {
	var out []fakeslack.MessageView
	for _, m := range thread {
		if m.BotID != "" {
			out = append(out, m)
		}
	}
	return out
}

// HasAction reports whether blocks, a message's blocks as JSON, hold an element with the action_id, at any
// depth: an input's element, a button in an actions block. Blocks that are absent or unreadable hold none.
func HasAction(blocks json.RawMessage, actionID string) bool { return findAction(blocks, actionID, "") }

// HasButton is HasAction for a button, which is what a prompt's Submit is and what a decided prompt no
// longer has.
func HasButton(blocks json.RawMessage, actionID string) bool {
	return findAction(blocks, actionID, "button")
}

// findAction reports whether blocks hold an element with the action_id and, if typ is not empty, that type.
func findAction(blocks json.RawMessage, actionID, typ string) bool {
	var v any
	if json.Unmarshal(blocks, &v) != nil {
		return false
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if t["action_id"] == actionID && (typ == "" || t["type"] == typ) {
				return true
			}
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		case []any:
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// OutboxID is the outbox ID courier tagged the message with, which the outbox looks a post up by after a
// crash, or "" if the message carries no such tag.
func OutboxID(m fakeslack.MessageView) string {
	if m.Metadata == nil || m.Metadata.EventType != chatslack.MetadataEventType {
		return ""
	}
	id, _ := m.Metadata.EventPayload["outbox_id"].(string)
	return id
}

// ChoicesState is the state of a checkboxes element with the options valued so selected, for
// Slack.Submit. A prompt's options are valued by the index of the choice.
func ChoicesState(values ...string) map[string]any {
	options := make([]map[string]string, len(values))
	for i, v := range values {
		options[i] = map[string]string{"value": v}
	}
	return map[string]any{"type": "checkboxes", "selected_options": options}
}

// TextState is the state of a plain-text input holding text, for Slack.Submit.
func TextState(text string) map[string]any {
	return map[string]any{"type": "plain_text_input", "value": text}
}

// Consistently checks cond now and then every pollInterval for d, and fails the first time it is false or
// returns an error. It is for the one thing a scenario must show by waiting: that something did not happen.
// It watches for the whole of d, so keep d short, and say in desc what was meant to hold. A cancelled ctx
// ends it with ctx's error.
func Consistently(ctx context.Context, d time.Duration, desc string, cond func(context.Context) (bool, error)) error {
	end := time.NewTimer(d)
	defer end.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		switch ok, err := cond(ctx); {
		case err != nil:
			return fmt.Errorf("checking that %s: %w", desc, err)
		case !ok:
			return fmt.Errorf("e2e: %s did not hold", desc)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("checking that %s: %w", desc, ctx.Err())
		case <-end.C:
			return nil
		case <-tick.C:
		}
	}
}
