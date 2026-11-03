package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// maxThreadPages is how many pages of a thread's replies ThreadBetween reads: with Config.PageLimit's default of 200, a
// thread window of more than a thousand replies is too long to read (chat.ErrThreadTooLong).
const maxThreadPages = 5

// ThreadBetween reads the replies of the thread threadTS in channel that are strictly after the message after and
// strictly before the message before (an empty one is no bound on that side) and returns the newest limit of them, oldest
// first, each decoded by historyMessage as a live message is, so that a message Run would drop is Ignored and Agent Kourier's
// own post is Own. conn is the "namespace/name" of the ChatConnection, which the Refs name. The thread's root is never
// among them, whether or not Slack returns it. dropped is how many older ones it left out, exactly.
//
// A window that is empty or inverted (after is not before before, by ts) is nothing, and makes no call. Otherwise it reads
// at most maxThreadPages pages of conversations.replies, one call each plus auth.test, with oldest and latest as the bounds
// and inclusive false, which Slack takes as exclusive bounds (the sandbox of 2026-10-05). Slack pages a thread oldest
// first with a forward cursor only, so the newest replies are the last pages, and a window that Slack still has pages of
// after the cap cannot be answered with the newest of it: it is an error wrapping chat.ErrThreadTooLong, with no
// messages and no sixth call, and a nil error always means the newest limit of the whole window. The result is sorted by
// ts, whatever order the pages came in.
//
// A rate limit is returned as the Client returns it, for Platform.Classify to give its RetryAfter; this method does not
// retry, and a thread it cannot read, or that is too long, is the caller's to do without.
//
// UNCONFIRMED(WP-04): whether Slack returns the parent when it is outside the bounds, and repeats it on every page: it
// is filtered by ts either way. The page cap is measured against the fake only (H-TM saw a window of one reply).
func (c *Client) ThreadBetween(ctx context.Context, conn, channel, threadTS, after, before string, limit int) (msgs []chat.HistoryMessage, dropped int, err error) {
	_, msgs, dropped, err = c.readThread(ctx, conn, channel, threadTS, after, before, limit)
	return msgs, dropped, err
}

// ThreadFromStart is ThreadBetween with no lower bound, and with the message that opened the thread first if Slack returned it,
// which limit does not count or cut. It is the one read: conversations.replies leads with the parent.
func (c *Client) ThreadFromStart(ctx context.Context, conn, channel, threadTS, before string, limit int) (msgs []chat.HistoryMessage, dropped int, err error) {
	root, msgs, dropped, err := c.readThread(ctx, conn, channel, threadTS, "", before, limit)
	if err != nil || root == nil {
		return msgs, dropped, err
	}
	return append([]chat.HistoryMessage{*root}, msgs...), dropped, nil
}

// readThread is the read of ThreadBetween and ThreadFromStart: root is the parent, if a page held it.
func (c *Client) readThread(ctx context.Context, conn, channel, threadTS, after, before string, limit int) (root *chat.HistoryMessage, msgs []chat.HistoryMessage, dropped int, err error) {
	if threadTS == "" || limit < 1 {
		return nil, nil, 0, fmt.Errorf("slack thread read: %w: a thread to read and a count of at least 1", chat.ErrInvalid)
	}
	if after != "" && before != "" && compareTS(after, before) >= 0 {
		return nil, nil, 0, nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ThreadBetween")
	defer func() { finishChat(ctx, op, "ThreadBetween", err) }()
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return nil, nil, 0, c.scrub("auth.test", err)
	}
	if auth.UserID == "" { // without it Agent Kourier cannot tell its own posts that arrive with no bot_id
		return nil, nil, 0, errors.New("slack auth.test: no bot user ID")
	}
	me := self{user: auth.UserID, bot: auth.BotID}
	var raw []slackgo.Message
	more, err := c.walkRange(ctx, channel, threadTS, tsRange{oldest: after, latest: before}, maxThreadPages, func(page []slackgo.Message) bool {
		for _, m := range page {
			switch { // the parent leads the replies; whether it is in the window is Slack's to say, and it is no reply
			case m.Timestamp != threadTS:
				raw = append(raw, m)
			case root == nil:
				first := threadMessage(me, conn, channel, m)
				root = &first
			}
		}
		return false
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if more {
		return nil, nil, 0, fmt.Errorf("slack thread read in %s: %w: more than %d pages of replies", channel, chat.ErrThreadTooLong, maxThreadPages)
	}
	slices.SortStableFunc(raw, func(a, b slackgo.Message) int { return compareTS(a.Timestamp, b.Timestamp) })
	if over := len(raw) - limit; over > 0 {
		raw, dropped = raw[over:], over
	}
	msgs = make([]chat.HistoryMessage, len(raw))
	for i, m := range raw {
		msgs[i] = threadMessage(me, conn, channel, m)
	}
	return root, msgs, dropped, nil
}

// threadMessage is a message of a thread, decoded by historyMessage. A bot's post may hold what it says in section and context blocks
// alone, so when its attachment has no text of its own, those words are its body, as they are in the post of a mention (mentionPost).
// This is for reading a thread: what a trigger matches, and what the dry run shows of a channel, stay the attachment's.
func threadMessage(me self, conn, channel string, m slackgo.Message) chat.HistoryMessage {
	if m.BotID != "" && (len(m.Attachments) == 0 || m.Attachments[0].Text == "") {
		if words := blockWords(m.Blocks); words != "" && words != m.Text {
			attachments := slices.Clone(m.Attachments)
			if len(attachments) == 0 {
				attachments = make([]slackgo.Attachment, 1)
			}
			attachments[0].Text = words
			m.Attachments = attachments
		}
	}
	return historyMessage(me, conn, channel, m)
}
