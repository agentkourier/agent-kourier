package teams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

const (
	// threadMaxPages is how many pages of a thread's replies one read asks Graph for: at graphPageTop a page, 1000 replies, the most
	// that the broker asks of a window in any case. A thread with more is chat.ErrThreadTooLong.
	threadMaxPages = 20
	// threadTimeout bounds one read of a thread as a whole, its root and every page.
	threadTimeout = 10 * time.Second
)

// errTeamNotKnown is the error of a read of a channel whose team's Microsoft Entra group the broker does not know.
var errTeamNotKnown = errors.New("teams: the Microsoft Entra group of the channel's team is not known yet: no activity of the channel " +
	"that names it has come to the bot")

// ThreadBetween implements chat.ThreadReader by Microsoft Graph: the replies of the thread t.Thread whose IDs are strictly between
// after and before (an empty one is no bound), the newest limit of them, oldest first, each decoded as the adapter decodes a live
// message (historyOf), with dropped the exact count of the older ones left out.
//
// The read is GET /teams/{group}/channels/{channel}/messages/{root}/replies with $top graphPageTop, and each page its @odata.nextLink
// names, at most threadMaxPages of them, with the app's Graph token, all within threadTimeout. The group is the Microsoft Entra group of
// the channel's team, from the channel's record in the store, which the receiver wrote from an activity of the channel: a channel that
// no activity naming its team has come from cannot be read (errTeamNotKnown), and that error is not chat.ErrNoThreadReads, so the turn
// says that the replies could not be read. Graph takes no bound of its own on the replies (only $top, as Microsoft documents it), so
// the window is cut here, by the messages' IDs, which order as their posts (compareIDs), and every page is read whatever the window:
// a thread that Graph has more pages of is chat.ErrThreadTooLong, with no messages, however few of them the window holds. The order
// Graph gives is not relied on: what was read is sorted. A message whose ID is not a Teams message ID is left out, uncounted.
//
// A window that is empty or inverted makes no call. A target with no channel or thread, a limit under 1, and a thread or a bound that is
// not a Teams message ID are chat.ErrInvalid, with no call. Graph's refusals are *GraphError, which Classify reads.
func (c *Client) ThreadBetween(ctx context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	_, msgs, dropped, err := c.readThread(ctx, t, after, before, limit, false)
	return msgs, dropped, err
}

// ThreadFromStart implements chat.ThreadReader: ThreadBetween with no lower bound, led by the message that opened the thread, which
// limit does not count or cut. It is read first, with GET …/messages/{root}: a root that Graph says is not there (404) is left out, and
// the thread starts with its first reply; any other failure of it fails the read. A thread too long to read comes back with its root.
func (c *Client) ThreadFromStart(ctx context.Context, t chat.Target, before string, limit int) ([]chat.HistoryMessage, int, error) {
	root, msgs, dropped, err := c.readThread(ctx, t, "", before, limit, true)
	if root == nil {
		return msgs, dropped, err
	}
	return append([]chat.HistoryMessage{*root}, msgs...), dropped, err
}

// ThreadRoot implements chat.RootReader: the message that opened the thread t.Thread, read with GET …/messages/{root} alone, the one
// call that ThreadFromStart begins with, decoded as it decodes it (historyOf), within threadTimeout. A root that Graph says is not there
// (404) is not found, with no error. The bounds and errors are ThreadBetween's: a target with no channel, or a thread that is not a
// Teams message ID, is chat.ErrInvalid with no call; a channel whose team is not known is errTeamNotKnown; Graph's refusals are
// *GraphError, which Classify reads. It is counted as ThreadRoot.
func (c *Client) ThreadRoot(ctx context.Context, t chat.Target) (msg chat.HistoryMessage, found bool, err error) {
	if t.Channel == "" || !isMessageID(t.Thread) {
		return chat.HistoryMessage{}, false, fmt.Errorf("teams thread root read: %w: a channel and a thread's message ID", chat.ErrInvalid)
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ThreadRoot")
	defer func() { c.finish(ctx, op, "ThreadRoot", err) }()
	ctx, cancel := context.WithTimeout(ctx, c.threadTimeout)
	defer cancel()
	group, err := c.teamGroup(ctx, t.Channel)
	if err != nil {
		return chat.HistoryMessage{}, false, err
	}
	root, err := c.readRoot(ctx, group, t)
	if err != nil || root == nil {
		return chat.HistoryMessage{}, false, err
	}
	return *root, true, nil
}

// readRoot reads the message that opened the thread t.Thread in the team of group, and nil if Graph says it is not there (404).
func (c *Client) readRoot(ctx context.Context, group string, t chat.Target) (*chat.HistoryMessage, error) {
	var m graphMessage
	switch err := c.graph.get(ctx, c.graph.resource("teams", group, "channels", t.Channel, "messages", t.Thread), &m); {
	case err == nil && m.ID == t.Thread:
		r := c.historyOf(t.Channel, t.Thread, m)
		return &r, nil
	case err == nil:
		return nil, errors.New("teams: Microsoft Graph answered the thread's first message with another")
	case isNotFound(err):
		return nil, nil
	default:
		return nil, err
	}
}

// readThread is the read of ThreadBetween and ThreadFromStart: root is the thread's first message, read when withRoot is set.
func (c *Client) readThread(ctx context.Context, t chat.Target, after, before string, limit int, withRoot bool) (root *chat.HistoryMessage,
	msgs []chat.HistoryMessage, dropped int, err error) {
	switch {
	case t.Channel == "" || !isMessageID(t.Thread) || limit < 1:
		return nil, nil, 0, fmt.Errorf("teams thread read: %w: a channel, a thread's message ID and a count of at least 1", chat.ErrInvalid)
	case (after != "" && !isMessageID(after)) || (before != "" && !isMessageID(before)):
		return nil, nil, 0, fmt.Errorf("teams thread read: %w: a bound that is not a message ID", chat.ErrInvalid)
	case after != "" && before != "" && compareIDs(after, before) >= 0:
		return nil, nil, 0, nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ThreadBetween")
	defer func() { c.finish(ctx, op, "ThreadBetween", err) }()
	ctx, cancel := context.WithTimeout(ctx, c.threadTimeout)
	defer cancel()
	group, err := c.teamGroup(ctx, t.Channel)
	if err != nil {
		return nil, nil, 0, err
	}
	if withRoot {
		if root, err = c.readRoot(ctx, group, t); err != nil {
			return nil, nil, 0, err
		}
	}
	var raw []graphMessage
	seen := map[string]bool{}
	first := c.graph.resource("teams", group, "channels", t.Channel, "messages", t.Thread, "replies") + fmt.Sprintf("?$top=%d", graphPageTop)
	more, err := c.graph.pages(ctx, first, threadMaxPages, func(page []graphMessage) bool {
		for _, m := range page {
			if !isMessageID(m.ID) || m.ID == t.Thread || seen[m.ID] ||
				(after != "" && compareIDs(m.ID, after) <= 0) || (before != "" && compareIDs(m.ID, before) >= 0) {
				continue
			}
			seen[m.ID] = true
			raw = append(raw, m)
		}
		return false
	})
	switch {
	case err != nil:
		return nil, nil, 0, err
	case more:
		return root, nil, 0, fmt.Errorf("teams thread read: %w: more than %d pages of replies", chat.ErrThreadTooLong, threadMaxPages)
	}
	slices.SortFunc(raw, func(a, b graphMessage) int { return compareIDs(a.ID, b.ID) })
	if over := len(raw) - limit; over > 0 {
		raw, dropped = raw[over:], over
	}
	msgs = make([]chat.HistoryMessage, len(raw))
	for i, m := range raw {
		msgs[i] = c.historyOf(t.Channel, t.Thread, m)
	}
	return root, msgs, dropped, nil
}

// isNotFound says whether err is Graph's 404.
func isNotFound(err error) bool {
	var ge *GraphError
	return errors.As(err, &ge) && ge.Status == http.StatusNotFound
}

// teamGroup is the Microsoft Entra group of the team of channel, as the channel's record names it, which Graph names the team by. It
// keeps the group for Permalink (knowGroup).
func (c *Client) teamGroup(ctx context.Context, channel string) (string, error) {
	rec, ok, err := c.convs.Lookup(ctx, c.conn, channel)
	switch {
	case err != nil:
		return "", fmt.Errorf("teams: reading the channel's record: %w", err)
	case !ok || rec.AADGroupID == "":
		return "", errTeamNotKnown
	}
	c.knowGroup(channel, rec.AADGroupID)
	return rec.AADGroupID, nil
}

// historyOf is the message m of the thread root in channel as the adapter would decode it live:
//   - a message of the bot's own app is Own, with its text;
//   - a person's (from.user, a Microsoft Entra object ID) has that ID as its user, its body as text (htmlText) and Mentioned when Graph
//     lists a mention of the bot's app; one with no text is Ignored, as a live post with none is dropped;
//   - another app's (from.application: an alert's Workflows post, say) is a bot's message, its sender the app's ID of the kind bot, its
//     body's text as Text and its cards' text (cardText) as Bot.Body, which is where an alert's post holds what it says; one with
//     neither is Ignored. Teams sends a bot no other app's post live, so this is what a read of the thread adds;
//   - anything else is Ignored, with only where it was: a messageType other than message (a system event), a deleted message, one
//     with no sender, or a sender of another shape.
func (c *Client) historyOf(channel, root string, m graphMessage) chat.HistoryMessage {
	at := chat.Ref{Platform: PlatformName, Conn: c.conn, Channel: channel, ID: m.ID, Thread: threadOf(root, m.ID)}
	ignored := chat.HistoryMessage{Message: chat.Inbound{At: at}, Ignored: true}
	if m.MessageType != "message" || m.DeletedDateTime != nil || m.From == nil {
		return ignored
	}
	text := m.Body.text()
	switch from := m.From; {
	case from.Application != nil && from.Application.ID == c.app:
		return chat.HistoryMessage{Message: chat.Inbound{At: at, Text: text}, Own: true}
	case from.Application != nil && from.Application.ID != "":
		card := m.cardsText()
		if text == "" && card == "" {
			return ignored
		}
		return chat.HistoryMessage{Message: chat.Inbound{At: at, Text: text, Mentioned: m.mentions(c.app),
			Bot: &chat.BotMessage{ID: from.Application.ID, Kind: v1alpha1.SenderKindBot, Body: card}}}
	case from.User != nil && entraID.MatchString(from.User.ID) && text != "":
		return chat.HistoryMessage{Message: chat.Inbound{At: at, User: from.User.ID, Text: text, Mentioned: m.mentions(c.app)}}
	}
	return ignored
}
