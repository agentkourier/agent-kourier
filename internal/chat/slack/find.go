package slack

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	slackgo "github.com/slack-go/slack"
)

// lookupSkew is how far before since Find looks: Slack stamps a message with its own clock,
// which may run behind courier's.
const lookupSkew = 5 * time.Minute

// Find looks for the message Post tagged with outboxID, in channel history or, if threadTS is
// set, in that thread's replies, from five minutes before since to now. It returns the message's ts.
//
// It reads every page to the end: it does not rely on the order Slack pages in, and a
// message anywhere in the window is found. It fails, instead of reporting "not found", when it
// cannot reach the end (Slack promises more and gives no cursor, or repeats one), because a
// wrong "not found" posts a duplicate. Only a message from courier's own bot user counts:
// another bot in the channel could forge the tag, and the ID is guessable.
//
// UNCONFIRMED(WP-04): the order Slack pages in (never relied on); that history and replies return
// the metadata of the app's own messages when asked with include_all_metadata, and need no
// registered event type; that a message in a thread shows in its replies with its metadata; that
// a bot's posts carry its auth.test user ID in user; that a message is visible in history as soon
// as chat.postMessage returns it (if not, a lookup right after a crash can miss it); and
// that five minutes allows for the clock skew.
func (c *Client) Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (ts string, found bool, err error) {
	ctx, op := telemetry.Start(ctx, "courier.chat.Find")
	defer func() { finishChat(ctx, op, "Find", err) }()
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return "", false, c.scrub("auth.test", err)
	}
	if auth.UserID == "" {
		return "", false, errors.New("slack auth.test: no bot user ID")
	}
	err = c.walk(ctx, channel, threadTS, since.Add(-lookupSkew), func(msgs []slackgo.Message) bool {
		for _, m := range msgs {
			if m.User == auth.UserID && m.Metadata.EventType == MetadataEventType && m.Metadata.EventPayload["outbox_id"] == outboxID {
				ts, found = m.Timestamp, true
				return true
			}
		}
		return false
	})
	if err != nil {
		return "", false, err
	}
	return ts, found, nil
}

// walk reads the pages of channel's history, or of threadTS's replies, from oldest to the end and gives each to
// visit, which ends the walk by returning true. It fails, instead of ending early, when it cannot reach the end
// (Slack promises more and gives no cursor, or repeats one): a caller that reads the whole window must not
// mistake a part of it for the whole.
func (c *Client) walk(ctx context.Context, channel, threadTS string, oldest time.Time, visit func([]slackgo.Message) (stop bool)) error {
	seen := map[string]bool{}
	for cursor := ""; ; {
		msgs, next, err := c.page(ctx, channel, threadTS, oldest, cursor)
		if err != nil {
			return err
		}
		if visit(msgs) || next == "" {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("slack lookup in %s: cursor %q repeats", channel, next)
		}
		seen[next] = true
		cursor = next
	}
}

// page reads one page of messages and the cursor of the next, empty at the end. It fails when
// Slack says there is more and gives no cursor. A zero oldest reads from the start of the channel.
func (c *Client) page(ctx context.Context, channel, threadTS string, oldest time.Time, cursor string) (msgs []slackgo.Message, next string, err error) {
	// No lower bound is no oldest parameter: Slack refuses a ts that is not after the epoch (invalid_ts_oldest), which
	// is what the zero time and the epoch itself format as.
	from := ""
	if oldest.Unix() > 0 {
		from = fmt.Sprintf("%d.%06d", oldest.Unix(), oldest.Nanosecond()/1000)
	}
	var more bool
	if threadTS == "" {
		var r *slackgo.GetConversationHistoryResponse
		r, err = c.api.GetConversationHistoryContext(ctx, &slackgo.GetConversationHistoryParameters{
			ChannelID: channel, Cursor: cursor, Oldest: from, Inclusive: true, Limit: c.pageSize, IncludeAllMetadata: true})
		if err != nil {
			return nil, "", c.scrub("conversations.history", err)
		}
		msgs, more, next = r.Messages, r.HasMore, r.ResponseMetaData.NextCursor
	} else {
		msgs, more, next, err = c.api.GetConversationRepliesContext(ctx, &slackgo.GetConversationRepliesParameters{
			ChannelID: channel, Timestamp: threadTS, Cursor: cursor, Oldest: from, Inclusive: true, Limit: c.pageSize, IncludeAllMetadata: true})
		if err != nil {
			return nil, "", c.scrub("conversations.replies", err)
		}
	}
	if more && next == "" {
		return nil, "", fmt.Errorf("slack lookup in %s: more messages but no next cursor", channel)
	}
	return msgs, next, nil
}
