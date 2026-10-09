package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// MetadataEventType is the event_type of the metadata on every message Agent Kourier posts. Its
// payload holds the outbox ID, so a post can be found again after a crash (spec 8.6).
const MetadataEventType = "agentkourier_message"

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
// UNCONFIRMED(WP-04): that Slack accepts event_type agentkourier_message with a string value in
// event_payload, and that chat.postMessage returns the ts of a message that carries metadata.
func (c *Client) Post(ctx context.Context, channel, threadTS string, content Content, outboxID string) (telemetryResult0 string, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Post")
	defer func() { finishChat(ctx, op, "Post", telemetryErr) }()
	tag, err := outboxTag(outboxID)
	if err != nil {
		return "", fmt.Errorf("slack post: %w", err)
	}
	opts, err := blockOptions(content.Blocks)
	if err != nil { // blocks that are not blocks never post
		return "", fmt.Errorf("slack post: %w: %w", store.ErrInvalid, err)
	}
	opts = append(opts, slackgo.MsgOptionText(content.Text, false), tag)
	opts = append(opts, inertOptions()...)
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	_, ts, err := c.api.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", c.scrub("chat.postMessage", err)
	}
	return ts, nil
}
