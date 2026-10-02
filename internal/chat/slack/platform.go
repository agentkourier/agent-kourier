package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"

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
}

// API is what the Platform asks of the Slack client: *Client, or a double of it.
type API interface {
	Run(ctx context.Context, sink Sink) error
	Post(ctx context.Context, channel, threadTS string, c Content, outboxID string) (string, error)
	Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (ts string, found bool, err error)
	Update(ctx context.Context, channel, ts string, c Content) error
	StartStream(ctx context.Context, channel, threadTS, recipientUser, markdown string) (string, error)
	AppendStream(ctx context.Context, channel, ts, markdown string) error
	StopStream(ctx context.Context, channel, ts, markdown string, blocks json.RawMessage, outboxID string) error
	PostEphemeral(ctx context.Context, channel, threadTS, user string, c Content) error
	React(ctx context.Context, channel, ts, name string) error
	IsChannelMember(ctx context.Context, channel, user string) (bool, error)
	IsBot(ctx context.Context, user string) (bool, error)
	ResolveUser(ctx context.Context, id string) (dialect.User, error)
}

var _ API = (*Client)(nil)

// Render is o as a Slack message, for the outbox until it stores the neutral model and delivers it through the
// Platform (WP-37 S5).
func Render(o chat.Outgoing) (Content, error) { return renderOutgoing(o) }

var _ chat.Platform = (*Platform)(nil)

// NewPlatform returns the adapter of the ChatConnection conn ("namespace/name"), which c is the client of.
func NewPlatform(c API, conn string) *Platform { return &Platform{c: c, conn: conn} }

func (p *Platform) ref(channel, thread, ts string) chat.Ref {
	return chat.Ref{Platform: PlatformName, Conn: p.conn, Channel: channel, Thread: thread, ID: ts}
}

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
	return in.sink.Message(ctx, chat.Inbound{
		At: in.p.ref(m.Channel, m.ThreadTS, m.TS), User: m.User, Text: m.Text,
		Mentioned: m.Mentioned, Direct: m.ChannelType == "im",
	})
}

// Interaction is a press as the neutral model has it, decoded from the Client's. It is for the broker's
// handler of the intake's presses, until the intake runs behind Run (WP-37 S6), which does the same.
func (p *Platform) Interaction(i Interaction) chat.Interaction { return p.interaction(i) }

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

// Update replaces the message at r with o. Slack takes the blocks again with every update, so an update
// always sends them.
func (p *Platform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	c, err := renderOutgoing(o)
	if err != nil {
		return err
	}
	return p.c.Update(ctx, r.Channel, r.ID, c)
}

// Stream starts a streaming message in t, shown to the user to, with the markdown first.
func (p *Platform) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	ts, err := p.c.StartStream(ctx, t.Channel, t.Thread, to, markdown(md, maxMarkdown))
	if err != nil {
		return nil, err
	}
	return &stream{p: p, ref: p.ref(t.Channel, t.Thread, ts)}, nil
}

type stream struct {
	p   *Platform
	ref chat.Ref
}

func (s *stream) Ref() chat.Ref { return s.ref }

func (s *stream) Append(ctx context.Context, md string) error {
	return notStreaming(s.p.c.AppendStream(ctx, s.ref.Channel, s.ref.ID, markdown(md, maxMarkdown)))
}

// Stop adds the last markdown, if any, and ends the stream with final's blocks, tagged with tag, in one call. What
// was streamed stays as the message's text: final's own fallback is not sent, since a stream has no separate
// text. A zero final sends no blocks, and leaves the message as it was streamed.
func (s *stream) Stop(ctx context.Context, md string, final chat.Message, tag string) error {
	var blocks json.RawMessage
	if final.Fallback != "" || len(final.Parts) > 0 {
		c, err := renderMessage(final)
		if err != nil {
			return err
		}
		blocks = c.Blocks
	}
	return notStreaming(s.p.c.StopStream(ctx, s.ref.Channel, s.ref.ID, markdown(md, maxMarkdown), blocks, tag))
}

// StreamAt is the stream that is the message at r.
func (p *Platform) StreamAt(r chat.Ref) chat.Stream { return &stream{p: p, ref: r} }

// notStreaming marks the error of a message that is not streaming, which Slack says by the code of its response:
// it ended, or it never was a stream (an update-path message's ts). Matched by the code, never by text.
// UNCONFIRMED(WP-04): the code real Slack gives for a ts that was never a stream. The fake gives
// message_not_in_streaming_state for both cases, so that is all that is tolerated.
func notStreaming(err error) error {
	var resp slackgo.SlackErrorResponse
	if errors.As(err, &resp) && resp.Err == "message_not_in_streaming_state" {
		return fmt.Errorf("%w: %w", chat.ErrNotStreaming, err)
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

// IsMember says whether user is in channel; it fails closed.
func (p *Platform) IsMember(ctx context.Context, channel, user string) (bool, error) {
	return p.c.IsChannelMember(ctx, channel, user)
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

// Fits says whether the prompt fits in one message, which is 50 blocks.
func (p *Platform) Fits(pr chat.Prompt) error {
	_, err := renderPrompt(pr)
	return err
}

// Capabilities is what Slack shows.
func (p *Platform) Capabilities() chat.Caps { return caps }

// Classify says what an error of the Client means for a retry: a rejected credential is fatal, the codes
// that say the same call will never work are permanent, with the reason in neutral terms and Slack's own
// code beside it, and anything else, a rate limit, a server error or a timeout, may pass.
func (p *Platform) Classify(op chat.Op, err error) chat.Verdict {
	if code := FatalAuthCode(err); code != "" {
		return chat.Verdict{Class: chat.FatalAuth, Code: code}
	}
	switch op {
	case chat.OpPost:
		if code := PermanentCode(err); code != "" {
			return chat.Verdict{Class: chat.Permanent, Reason: postReason[code], Code: code}
		}
	case chat.OpReact:
		if code := ReactionPermanentCode(err); code != "" {
			return chat.Verdict{Class: chat.Permanent, Reason: reactionReason[code], Code: code}
		}
	}
	v := chat.Verdict{Class: chat.Transient}
	if limited := new(*slackgo.RateLimitedError); errors.As(err, limited) {
		v.RetryAfter = max((*limited).RetryAfter, time.Second) // one that names no wait is still one
	}
	return v
}

// The neutral reason of each code PermanentCode and ReactionPermanentCode give. A code not here is Other.
var (
	postReason = map[string]chat.Reason{
		"channel_not_found": chat.ReasonChannelGone, "is_archived": chat.ReasonChannelGone, "not_in_channel": chat.ReasonChannelGone,
		"msg_too_long": chat.ReasonTooLong, "invalid_blocks": chat.ReasonInvalidContent, "invalid_blocks_format": chat.ReasonInvalidContent,
	}
	reactionReason = map[string]chat.Reason{
		"message_not_found": chat.ReasonChannelGone, "channel_not_found": chat.ReasonChannelGone, "is_archived": chat.ReasonChannelGone,
		"not_in_channel": chat.ReasonChannelGone, "no_item_specified": chat.ReasonInvalidContent, "bad_timestamp": chat.ReasonInvalidContent,
		"invalid_name":  chat.ReasonInvalidContent,
		"thread_locked": chat.ReasonOther, "not_reactable": chat.ReasonOther, "too_many_reactions": chat.ReasonOther, "too_many_emoji": chat.ReasonOther,
	}
)

// interaction is a press as the neutral model has it: the answers are read from the state of the controls, by
// the block and action IDs that renderPrompt made, and each only as the control it was asked in.
func (p *Platform) interaction(i Interaction) chat.Interaction {
	return chat.Interaction{
		User: i.User, At: p.ref(i.Channel, i.ThreadTS, i.MessageTS), PromptID: i.Value, Answers: answers(i.Inputs),
	}
}

var blockIndex = regexp.MustCompile(`^q(\d{1,4})$`)

// answers reads the answer of each question from the inputs. An element of another type than its action ID
// says, or an input that holds nothing, is an unanswered question: an answer is never one the person did not
// give.
func answers(inputs map[string]map[string]Input) []chat.Answer {
	var out []chat.Answer
	for block, actions := range inputs {
		m := blockIndex.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		q, _ := strconv.Atoi(m[1])
		for id, in := range actions {
			control, ok := controlOf(id, q)
			if !ok || controlElement[control] != in.Type || len(in.Values) == 0 {
				continue
			}
			values := slices.Clone(in.Values)
			if control == controlNumbers {
				values = positions(in.Values)
			}
			out = append(out, chat.Answer{Question: q, Values: values})
		}
	}
	slices.SortFunc(out, func(a, b chat.Answer) int { return cmp.Compare(a.Question, b.Question) })
	return out
}

// controlOf is the control of the action ID id of question q, or ok false if it is not one of that question.
func controlOf(id string, q int) (byte, bool) {
	if len(id) < 2 || id[1:] != strconv.Itoa(q) {
		return 0, false
	}
	_, ok := controlElement[id[0]]
	return id[0], ok
}

// positions turns the numbers a person typed beside a numbered list (1-based, separated by commas or
// spaces) into the indices the neutral model answers choices with. A token that is not a number stays as
// it was typed, for the caller to find that it is no choice.
func positions(typed []string) []string {
	var out []string
	for _, v := range typed {
		for _, tok := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			if n, err := strconv.Atoi(tok); err == nil && n >= 1 {
				out = append(out, strconv.Itoa(n-1))
			} else {
				out = append(out, tok)
			}
		}
	}
	return out
}
