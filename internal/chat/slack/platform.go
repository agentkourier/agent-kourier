package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// PlatformName is the name Refs of Slack messages carry: the first part of chat.MessageID, which is how
// the store already names a Slack message.
const PlatformName = "slack"

// Platform is Slack as a chat.Platform: the Client's calls, with the neutral model rendered as Block Kit and
// every text neutralised for Slack (escape.go). It is the adapter of one ChatConnection. It is safe for
// concurrent use.
type Platform struct {
	c    API
	conn string
	log  *slog.Logger

	// markdown is latched when Slack refused the chunks mode and took the markdown_text one: every stream
	// from then on starts in the mode that works, with no failed call first. warned makes the log of it one.
	markdown atomic.Bool
	warned   sync.Once

	// statusOff is latched when Slack refused the assistant status for good (MarkBusy): the platform makes no more calls for
	// it. statusWarned makes the log of it one.
	statusOff    atomic.Bool
	statusWarned sync.Once
}

// API is what the Platform asks of the Slack client: *Client, or a double of it.
type API interface {
	Run(ctx context.Context, sink Sink) error
	Post(ctx context.Context, channel, threadTS string, c Content, outboxID string) (string, error)
	Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (ts string, found bool, err error)
	Update(ctx context.Context, channel, ts string, c Content) error
	StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string, mode StreamMode) (string, error)
	AppendStream(ctx context.Context, channel, ts, markdown string, mode StreamMode) error
	AppendStep(ctx context.Context, channel, ts string, u TaskUpdate) error
	StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string, mode StreamMode) error
	PostEphemeral(ctx context.Context, channel, threadTS, user string, c Content) error
	React(ctx context.Context, channel, ts, name string) error
	Unreact(ctx context.Context, channel, ts, name string) error
	IsChannelMember(ctx context.Context, channel, user string) (bool, error)
	IsInAnyUserGroup(ctx context.Context, groups []string, user string) (bool, error)
	GroupsReadable(ctx context.Context, groups []string) error
	ChannelAccess(ctx context.Context, channel string) (chat.Access, error)
	IsBot(ctx context.Context, user string) (bool, error)
	ResolveUser(ctx context.Context, id string) (dialect.User, error)
	ThreadBetween(ctx context.Context, conn, channel, threadTS, after, before string, limit int) ([]chat.HistoryMessage, int, error)
	ThreadFromStart(ctx context.Context, conn, channel, threadTS, before string, limit int) ([]chat.HistoryMessage, int, error)
	SetThreadStatus(ctx context.Context, channel, threadTS, status string) error
	WorkspaceURL() string
}

var _ API = (*Client)(nil)

// Render is o as the Slack message that Post sends: its text and blocks. Nothing in Agent Kourier delivers through it (the
// outbox stores the neutral model and the Platform renders it at delivery); it is for a test or a tool that shows what
// Slack would be sent.
func Render(o chat.Outgoing) (Content, error) { return renderOutgoing(o) }

var _ chat.Platform = (*Platform)(nil)

// NewPlatform returns the adapter of the ChatConnection conn ("namespace/name"), which c is the client of.
func NewPlatform(c API, conn string) *Platform {
	return &Platform{c: c, conn: conn, log: slog.Default()}
}

// WithLogger sets the logger of the Platform, which logs the one thing it reports on its own: Slack's refusal of the
// chunks mode (Stream). It returns p, for the call that builds it.
func (p *Platform) WithLogger(l *slog.Logger) *Platform {
	if l != nil {
		p.log = l
	}
	return p
}

func (p *Platform) ref(channel, thread, ts string) chat.Ref {
	return chat.Ref{Platform: PlatformName, Conn: p.conn, Channel: channel, Thread: thread, ID: ts}
}

// RefAt implements chat.Platform: the Ref of the message with the ts id in a channel and thread.
func (p *Platform) RefAt(channel, thread, id string) chat.Ref { return p.ref(channel, thread, id) }

// Run connects over Socket Mode and gives sink what people write and press, until ctx ends or the connection
// fails for good (Client.Run).
func (p *Platform) Run(ctx context.Context, sink chat.Sink) error {
	return p.c.Run(ctx, inbound{p: p, sink: sink})
}

// inbound is the Sink of the Client, which hands the neutral sink what it decodes.
type inbound struct {
	p    *Platform
	sink chat.Sink
}

func (in inbound) Message(ctx context.Context, m Message) error {
	return in.sink.Message(ctx, InboundOf(in.p.conn, m))
}

// InboundOf is m as the neutral model has it, on the connection conn ("namespace/name"). A bot's message is flattened
// from its first legacy attachment into the neutral fields: the attachment's title is the Title, its title_link the
// Link, its text the Body, its footer the Footer and its fallback the Fallback; the message's top-level text stays Text. The sender is the message's bot_id, of
// the kind bot. The live path and the
// history (Client.History) both make an Inbound this way, and it is exported so that a test of a trigger can make
// the Inbound the intake would have been given for a message, without copying the conversion.
func InboundOf(conn string, m Message) chat.Inbound {
	msg := chat.Inbound{
		At:   chat.Ref{Platform: PlatformName, Conn: conn, Channel: m.Channel, Thread: m.ThreadTS, ID: m.TS},
		User: m.User, Text: m.Text, Mentioned: m.Mentioned, Direct: m.ChannelType == "im",
	}
	if m.Bot.ID != "" {
		msg.Bot = &chat.BotMessage{ID: m.Bot.ID, Kind: v1alpha1.SenderKindBot, Title: m.Bot.Title, Link: m.Bot.TitleLink,
			Fallback: m.Bot.Fallback, Footer: m.Bot.Footer, Body: m.Bot.Text}
	}
	return msg
}

// AcceptsBot is BotFilter: the neutral sink's answer, and no if it gives none (chat.BotFilter), so that a sink that
// knows nothing of bots is given none of their messages.
func (in inbound) AcceptsBot(channel, botID string) bool {
	f, ok := in.sink.(chat.BotFilter)
	return ok && f.AcceptsBot(channel, botID)
}

// AcceptsBotMention is BotMentionFilter: the neutral sink's answer, and no if it gives none (chat.BotMentionFilter).
func (in inbound) AcceptsBotMention(channel, botID string) bool {
	f, ok := in.sink.(chat.BotMentionFilter)
	return ok && f.AcceptsBotMention(channel, botID)
}

var (
	_ BotFilter        = inbound{}
	_ BotMentionFilter = inbound{}
)

func (in inbound) Interaction(ctx context.Context, i Interaction) error {
	return in.sink.Interaction(ctx, in.p.interaction(i))
}

// Post says o in t, tagged with tag, and returns the message.
func (p *Platform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	c, err := renderOutgoing(o)
	if err != nil {
		return chat.Ref{}, err
	}
	ts, err := p.c.Post(ctx, t.Channel, t.Thread, c, tag)
	if err != nil {
		return chat.Ref{}, err
	}
	return p.ref(t.Channel, t.Thread, ts), nil
}

// Find returns the message in t tagged with tag, posted no earlier than about since.
func (p *Platform) Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error) {
	ts, found, err := p.c.Find(ctx, t.Channel, t.Thread, tag, since)
	if err != nil || !found {
		return chat.Ref{}, false, err
	}
	return p.ref(t.Channel, t.Thread, ts), true, nil
}

// ThreadBetween implements chat.ThreadReader over the Client's (Client.ThreadBetween). A target that names no thread is
// chat.ErrInvalid.
func (p *Platform) ThreadBetween(ctx context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	return p.c.ThreadBetween(ctx, p.conn, t.Channel, t.Thread, after, before, limit)
}

// ThreadFromStart implements chat.ThreadReader over the Client's (Client.ThreadFromStart). A target that names no thread is
// chat.ErrInvalid.
func (p *Platform) ThreadFromStart(ctx context.Context, t chat.Target, before string, limit int) ([]chat.HistoryMessage, int, error) {
	return p.c.ThreadFromStart(ctx, p.conn, t.Channel, t.Thread, before, limit)
}

// Update replaces the message at r with o. Slack takes the blocks again with every update, so an update
// always sends them.
func (p *Platform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	c, err := renderOutgoing(o)
	if err != nil {
		return err
	}
	return wouldDropText(p.c.Update(ctx, r.Channel, r.ID, c))
}

// wouldDropText marks the error of an update that takes the text from a message that holds only text, which Slack says by the
// code of its response. Slack documents block_mismatch as "Rich-text blocks cannot be replaced with non-rich-text blocks", and a
// probe on 2026-11-03 in a sandbox workspace, by chat.update, found what that means. The message's blocks must all be rich text
// (a markdown block and a stream's text are stored as rich_text), and the update's blocks must hold no markdown or rich_text
// block. A message with a card or any other block beside its text takes such an update and loses the text, one posted with text
// and no blocks takes it too, and no blocks at all, or an empty list, is no such update. Five tries, 2 seconds apart, were all
// refused, and an update with text then passed. Matched by the code, never by text.
func wouldDropText(err error) error {
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && resp.Err == "block_mismatch" {
		return fmt.Errorf("%w: %w", chat.ErrWouldDropText, err)
	}
	return err
}

// Ephemeral shows m to user alone.
func (p *Platform) Ephemeral(ctx context.Context, t chat.Target, user string, m chat.Message) error {
	c, err := renderMessage(m)
	if err != nil {
		return err
	}
	return p.c.PostEphemeral(ctx, t.Channel, t.Thread, user, c)
}

// React puts the reaction name on the message at r.
func (p *Platform) React(ctx context.Context, r chat.Ref, name string) error {
	return p.c.React(ctx, r.Channel, r.ID, name)
}

// Unreact takes the reaction name off the message at r.
func (p *Platform) Unreact(ctx context.Context, r chat.Ref, name string) error {
	return p.c.Unreact(ctx, r.Channel, r.ID, name)
}

// IsMember says whether user is in channel; it fails closed.
func (p *Platform) IsMember(ctx context.Context, channel, user string) (bool, error) {
	return p.c.IsChannelMember(ctx, channel, user)
}

// IsInAnyGroup says whether user is in any of the Slack user groups with the IDs groups; it fails closed.
func (p *Platform) IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) {
	return p.c.IsInAnyUserGroup(ctx, groups, user)
}

// GroupsReadable says whether the bot can read every one of the Slack user groups with the IDs groups.
func (p *Platform) GroupsReadable(ctx context.Context, groups []string) error {
	return p.c.GroupsReadable(ctx, groups)
}

// ChannelAccess says whether the bot is in channel and whether channel is archived, from one conversations.info call.
func (p *Platform) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	return p.c.ChannelAccess(ctx, channel)
}

// IsBot says whether user is a bot; it fails closed.
func (p *Platform) IsBot(ctx context.Context, user string) (bool, error) {
	return p.c.IsBot(ctx, user)
}

// ResolveUser returns the user with the Slack ID id.
func (p *Platform) ResolveUser(ctx context.Context, id string) (chat.User, error) {
	u, err := p.c.ResolveUser(ctx, id)
	if err != nil {
		return chat.User{}, err
	}
	return chat.User{ID: u.ID, Name: u.Name, Email: u.Email}, nil
}

// Fits says whether the prompt fits in one message: 50 blocks, and for a prompt of tools also the limits of a
// tool's name and arguments and of the arguments of all its tools together (renderToolPrompt).
func (p *Platform) Fits(pr chat.Prompt) error {
	_, err := renderPrompt(pr)
	return err
}

// Capabilities is what Slack shows.
func (p *Platform) Capabilities() chat.Caps { return caps }
