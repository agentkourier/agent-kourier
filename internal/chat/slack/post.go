package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	slackgo "github.com/slack-go/slack"
)

// MetadataEventType is the event_type of the metadata on every message courier posts. Its
// payload holds the outbox ID, so a post can be found again after a crash (spec 8.6).
const MetadataEventType = "courier_message"

// Content is what a post says: its text, and optionally Block Kit blocks as JSON.
type Content struct {
	Text   string          `json:"text"`
	Blocks json.RawMessage `json:"blocks,omitempty"`
}

// outboxTag returns the option that tags a message with metadata naming outboxID. An empty ID
// is refused: the tag is what Find looks for.
func outboxTag(outboxID string) (slackgo.MsgOption, error) {
	if outboxID == "" {
		return nil, errors.New("an outbox ID is required to tag the message")
	}
	return slackgo.MsgOptionMetadata(slackgo.SlackMetadata{EventType: MetadataEventType, EventPayload: map[string]any{"outbox_id": outboxID}}), nil
}

// Post posts c to channel, in the thread threadTS if set, tagged with metadata that names
// outboxID, and returns the message's ts. The tag is what Find looks for, so outboxID is required.
// A failure does not say whether the message was posted: the caller finds it before posting again.
// Its error holds no token.
//
// UNCONFIRMED(WP-04): that Slack accepts event_type courier_message with a string value in
// event_payload, and that chat.postMessage returns the ts of a message that carries metadata.
func (c *Client) Post(ctx context.Context, channel, threadTS string, content Content, outboxID string) (string, error) {
	tag, err := outboxTag(outboxID)
	if err != nil {
		return "", fmt.Errorf("slack post: %w", err)
	}
	opts, err := blockOptions(content.Blocks)
	if err != nil {
		return "", fmt.Errorf("slack post: %w", err)
	}
	opts = append(opts, slackgo.MsgOptionText(content.Text, false), tag)
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	_, ts, err := c.api.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", c.scrub("chat.postMessage", err)
	}
	return ts, nil
}
