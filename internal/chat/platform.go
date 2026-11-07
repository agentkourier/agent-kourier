package chat

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// Inbound is a message written in a conversation Agent Kourier is in: by a person, or, if Bot is set, by a bot.
type Inbound struct {
	// At is the message, with its thread in At.Thread.
	At Ref
	// User is who wrote it. A message of a bot that has no user of its own (an incoming webhook's) has none.
	User string
	// Text is what they wrote, verbatim, with any mention of Agent Kourier in it. For a bot's message it is the
	// message's own text, which is often empty: its content is in Bot.
	Text string
	// Mentioned says the message mentions Agent Kourier. Direct says it is in a conversation of its own with
	// Agent Kourier, a direct message, where no mention is needed. The adapter decides both, so the routing
	// that uses them knows nothing of a platform's syntax. A bot's message is Mentioned only when the
	// platform says the bot mentioned Agent Kourier; its other event of the same post is not (D36).
	Mentioned, Direct bool
	// Bot is set when a bot posted the message, and only then. A platform forwards a bot's message only
	// if the Sink takes it (BotFilter), and never Agent Kourier's own.
	Bot *BotMessage
}

// BotMessage is what a bot's message carries beyond a person's: the sender, by its ID and kind and never its display name
// (D36), and the content of its message flattened into the fields a botMessage trigger's regexes read
// (v1alpha1.MessageField). Every value is the raw text the platform delivered: untrusted (R2), and neither
// rendered nor unescaped.
type BotMessage struct {
	// ID is the sender's ID on the platform (Slack: the bot ID), and Kind what kind of sender that ID names. A trigger takes
	// a message only when both equal its from.id and from.kind; an adapter sets the kind of every sender it forwards, and an
	// empty one means a bot.
	ID   string
	Kind v1alpha1.SenderKind
	// Title, Link, Body, Footer and Fallback are the heading, the heading's URL, the main text, the small print and the
	// plain-text summary of the message's structured part (Slack: its first legacy attachment); the message's top-level text
	// is Inbound.Text. The adapter documents how it flattens its native message into them.
	Title, Link, Fallback, Footer, Body string
}

// IsSender says whether the message's sender is the one src names: the IDs are equal and so are the kinds, an unset kind
// being a bot on either side.
func (b BotMessage) IsSender(src v1alpha1.BotSource) bool {
	return b.ID == src.ID && cmp.Or(b.Kind, v1alpha1.SenderKindBot) == cmp.Or(src.Kind, v1alpha1.SenderKindBot)
}

// Field returns the value of the named message field: Text from the message itself, and the others from the
// bot's attachment. A message with no Bot, or a name that is no field, has the empty string.
func (m Inbound) Field(f v1alpha1.MessageField) string {
	if f == v1alpha1.FieldText {
		return m.Text
	}
	if m.Bot == nil {
		return ""
	}
	switch f {
	case v1alpha1.FieldTitle:
		return m.Bot.Title
	case v1alpha1.FieldLink:
		return m.Bot.Link
	case v1alpha1.FieldFallback:
		return m.Bot.Fallback
	case v1alpha1.FieldFooter:
		return m.Bot.Footer
	case v1alpha1.FieldBody:
		return m.Bot.Body
	}
	return ""
}

// Permalinker is the part of a Platform that builds a link to a message from what the platform learned when it
// connected, with no call of its own, so it is fit for the acknowledgement path, where a call is not allowed. A platform
// that makes no links says false.
type Permalinker interface {
	// Permalink returns the link to the message r, and false if the platform cannot make one: it has not connected
	// yet, or r does not name a message of the platform's.
	Permalink(r Ref) (string, bool)
}

// ThreadReader is the part of a Platform that reads the replies in a thread that people wrote between two messages,
// which a mention-only Binding shows an agent as the context of the mention (spec 8.2). A platform that cannot read a
// thread, for good or at the moment, says ErrNoThreadReads, so a caller asks and goes on without the context when it is
// told no.
type ThreadReader interface {
	// ThreadBetween returns the messages of the thread t.Thread whose IDs are strictly after after and strictly before
	// before, oldest first, each decoded as the adapter decodes a live message: a message that Run would drop is Ignored,
	// and Agent Kourier's own post is Own. The thread's root is never among them. An empty bound is no bound on that side.
	// A window that is empty or inverted (after is not before before) returns nothing and makes no call.
	//
	// At most limit messages come back, the newest of the window, and dropped says how many were left out: exactly the
	// older ones over limit. Both count every message, Agent Kourier's own, Ignored ones and other bots' included, so a caller
	// that wants N eligible replies passes a larger limit and filters. A window the platform cannot read to its end in
	// one call is no partial answer: it is an error wrapping ErrThreadTooLong, so a nil error always means the newest
	// of the whole window. Check ErrThreadTooLong and ErrNoThreadReads with errors.Is before Classify, which does not
	// know them; any other error classifies as the platform's (Classify, OpOther), a rate limit with its RetryAfter.
	ThreadBetween(ctx context.Context, t Target, after, before string, limit int) (msgs []HistoryMessage, dropped int, err error)
	// ThreadFromStart is ThreadBetween with no lower bound, for a turn that carries the thread from its start, and with the message
	// that opened the thread first, which limit does not count or cut. A thread whose first message the platform no longer has
	// starts with its first reply. ErrThreadTooLong may come with what opened the thread, and nothing else.
	ThreadFromStart(ctx context.Context, t Target, before string, limit int) (msgs []HistoryMessage, dropped int, err error)
}

// Sink receives what Platform.Run decodes. A platform may call it from several goroutines at once, so a Sink is safe for
// concurrent use; a platform keeps the events of one channel in order and calls for one channel one at a time.
type Sink interface {
	// Message takes m. An error means it was not taken, and the platform delivers it again.
	Message(ctx context.Context, m Inbound) error
	// Interaction takes i. An error leaves the event unacknowledged, which is the person's only sign
	// that the press failed; nothing may rely on a platform delivering it again.
	Interaction(ctx context.Context, i Interaction) error
}

// BotFilter is an optional interface of a Sink that takes the messages of bots. A platform asks it before it
// forwards one, so a bot that no trigger can match is dropped at the adapter and never reaches the Sink (D36),
// and a Sink that does not implement it is given none: a platform fails closed. The platform never forwards
// Agent Kourier's own messages, whatever the answer. The answer is a routing question, so the adapter holds no copy
// of the config.
type BotFilter interface {
	// AcceptsBot says whether a botMessage trigger of the Binding that owns channel names botID. It is a first cut by ID
	// alone, so that a sender no trigger can match is dropped at the adapter: the trigger's own match also holds the kind.
	AcceptsBot(channel, botID string) bool
}

// BotMentionFilter is an optional interface of a Sink that takes bots' mentions. A platform asks it before it forwards
// one, and a Sink that does not implement it is given none: a platform fails closed. Agent Kourier's own mention is never
// forwarded, whatever the answer.
type BotMentionFilter interface {
	// AcceptsBotMention says whether a mention by the bot botID in channel is to be taken.
	AcceptsBotMention(channel, botID string) bool
}

// Stream is a message that grows while an agent writes it. The markdown that Append and Stop take is
// untrusted, as Body.Markdown is.
type Stream interface {
	// Ref is the streaming message.
	Ref() Ref
	// Append adds markdown to the message.
	Append(ctx context.Context, markdown string) error
	// Step shows the progress of one unit of the agent's work in the message, in the place in the text where
	// the stream is now: a step with an ID the stream has not seen is begun there, and one it has seen is moved
	// to the state it now has, where it stands. A platform shows it however it does, and Caps.Steps says
	// whether it does: without it Step returns ErrNoSteps and shows nothing, so a caller asks first. The
	// Title and Detail are untrusted literal text (see Step). A step that is never finished stays as it was, so a
	// caller that ends a stream finishes its steps first (StepEnded for one with no outcome). Like Append, Step
	// says ErrNotStreaming of a message that is not streaming.
	Step(ctx context.Context, s Step) error
	// Stop adds the last markdown, if there is any, and ends the stream, tagged with tag as Post tags a message,
	// so that Find can tell a message that was made from one that was not. Both are one call: the last of the
	// text and the end of the stream come together.
	//
	// What was streamed stays as the message's content, and final is shown under it: a platform appends it, and
	// does not replace the streamed text with it. A caller must never put the streamed text in final, or the
	// message shows it twice. final is for what a stream cannot carry (buttons, say), and the zero Message adds
	// nothing. A caller that wants other content than what was streamed ends the stream with the zero Message and
	// then Updates the message.
	Stop(ctx context.Context, markdown string, final Message, tag string) error
}

// Errors an adapter returns for what a platform cannot or will not do.
var (
	// ErrNoStreams is returned by Stream on a platform without streams (Caps.Streams false).
	ErrNoStreams = errors.New("chat: the platform has no streaming messages")
	// ErrNotStreaming is wrapped by the error of an Append or a Stop of a message that is not streaming: it ended,
	// or it never was a stream. The caller that is finishing a stream it did not start tolerates it.
	ErrNotStreaming = errors.New("chat: the message is not streaming")
	// ErrWouldDropText is wrapped by the error of an Update that the platform refuses because the message holds only text
	// and the new content has none: the text can be changed, and not taken away. The message is as it was, and the same
	// content is refused however often it is tried, until a call has text to put in its place, which is accepted.
	ErrWouldDropText = errors.New("chat: the update would take away the text that is all the message holds")
	// ErrTooLarge is returned by Fits for a prompt that does not fit in one message even in its
	// plainest form. No retry mends it, and the caller reports the pause as unusable.
	ErrTooLarge = errors.New("chat: the prompt does not fit in one message")
	// ErrNoThreadReads is returned by ThreadBetween of a platform that cannot read a thread, for good or at the moment.
	// The caller goes on without the thread's earlier replies; no retry mends it.
	ErrNoThreadReads = errors.New("chat: the platform cannot read the replies of a thread")
	// ErrThreadTooLong is wrapped by the error of a ThreadBetween whose window is longer than the platform will read in
	// one call. No retry mends it: the same window is as long the next time. The caller goes on without the replies, as
	// for ErrNoThreadReads.
	ErrThreadTooLong = errors.New("chat: the window of the thread is too long to read")
	// ErrGroupsMisconfigured is wrapped by the error of an IsInAnyGroup that can never succeed until an operator changes
	// the configuration: a group that does not exist or is disabled, an app that is not allowed to read groups (it was
	// installed before it asked for the scope), a workspace that has no groups. A caller checks it with errors.Is and ends
	// the press at once instead of retrying. An error that does not wrap it may pass on another try (a timeout, a server
	// error, a rate limit), and a cancelled context is never wrapped in it.
	ErrGroupsMisconfigured = errors.New("chat: the approver groups cannot be read until the platform's configuration is fixed")
	// ErrChannelNotFound is wrapped by the error of a ChannelAccess for a channel the platform says is not there, or
	// is not there for Agent Kourier: it never existed, it was deleted, or (on a platform that hides a private
	// conversation from a member who is not in it) it is private and Agent Kourier is not in it. A caller checks it with
	// errors.Is and does not retry: the same channel is as missing on the next try, until an operator changes the
	// Binding or invites the bot. An error that does not wrap it may pass on another try, and a cancelled context is
	// never wrapped in it.
	ErrChannelNotFound = errors.New("chat: the platform has no such channel, or none that Agent Kourier can see")
	// ErrMissingScope is wrapped by the error of a ChannelAccess or a GroupsReadable that the platform refuses because the app
	// was installed without the permission the read needs (Slack: missing_scope). A caller checks it with errors.Is and does not
	// retry: only an operator who adds the permission and reinstalls the app mends it. An error that does not wrap it may pass
	// on another try, and a cancelled context is never wrapped in it.
	ErrMissingScope = errors.New("chat: the app lacks a permission the platform needs for this read")
	// ErrGroupNotFound is wrapped by the error of a GroupsReadable for a group the platform says is not there, or not there for
	// Agent Kourier (Slack: no_such_subteam; whether a disabled group is the same is not confirmed). It also wraps
	// ErrGroupsMisconfigured. A caller checks it with errors.Is and does not retry: the same group is as missing on the next try,
	// until an operator changes the Binding or the workspace. A cancelled context is never wrapped in it.
	ErrGroupNotFound = errors.New("chat: the platform has no such group, or none that Agent Kourier can see")
)

// GroupError is the error of a GroupsReadable for one group: Group is the platform's ID of the group that could not be read, as the
// caller gave it, and Err is why. A caller finds it with errors.As, and Err's sentinels with errors.Is on the GroupError.
type GroupError struct {
	// Group is the ID of the group that could not be read.
	Group string
	// Err is the platform's error for that group. It holds no token.
	Err error
}

// Error describes the failure, with the group's ID.
func (e *GroupError) Error() string { return fmt.Sprintf("group %s: %v", e.Group, e.Err) }

// Unwrap returns the platform's error.
func (e *GroupError) Unwrap() error { return e.Err }

// Access is how Agent Kourier stands to one conversation, as Platform.ChannelAccess reads it.
type Access struct {
	// Member says Agent Kourier is in the conversation. It is the platform's own flag, and independent of Archived: a
	// platform may say true for an archived conversation that Agent Kourier was in when it was archived.
	Member bool
	// Archived says the platform has archived the conversation.
	Archived bool
}

// Platform is a chat platform: Slack today. An adapter implements it, and nothing else in Agent Kourier names
// the platform it adapts. Every method is safe for concurrent use.
//
// Every method that sends text keeps the contract of the package comment, and the adapter passes
// chattest.Conformance to show it does.
type Platform interface {
	// Run connects to the platform and delivers what people write and press to sink, until ctx ends or
	// the connection cannot be kept (an error that Classify says is FatalAuth is a rejected credential,
	// which no restart mends).
	Run(ctx context.Context, sink Sink) error

	// Post says o in t, tagged with tag (the outbox's ID for the post, kept as the platform's own
	// metadata, so that Find can recognise the message), and returns where it landed. o is rendered
	// for the platform here, at delivery. A malformed o is an error wrapping ErrInvalid.
	Post(ctx context.Context, t Target, o Outgoing, tag string) (Ref, error)
	// Find returns the message in t tagged with tag, posted no earlier than about since, if there is one.
	// Its error is not "not found": that is found false.
	Find(ctx context.Context, t Target, tag string, since time.Time) (Ref, bool, error)
	// Update replaces the content of the message at r with o. A platform may keep a message's text from being replaced by
	// content with none, and says so with ErrWouldDropText. A caller that does not know what the message holds can give
	// content with no text and take that answer, but must not count on it: a platform may also take such content, and the
	// text is then gone.
	Update(ctx context.Context, r Ref, o Outgoing) error
	// RefAt is the Ref of the message with the platform's ID id in a conversation, which a caller that recorded the ID alone
	// (the outbox, the interactions) gets back whole, so that no caller writes a platform's fields by hand.
	RefAt(channel, thread, id string) Ref
	// Stream starts a streaming message in t shown to the user to, beginning with markdown. It returns
	// ErrNoStreams on a platform without them.
	Stream(ctx context.Context, t Target, to, markdown string) (Stream, error)
	// StreamAt is the stream that is the message at r, which another run started and a recovery has the Ref of: it
	// makes no call, and its Append and Stop say ErrNotStreaming if the message is not streaming.
	StreamAt(r Ref) Stream
	// Ephemeral shows m to user alone, in t. Nothing keeps it.
	Ephemeral(ctx context.Context, t Target, user string, m Message) error
	// React puts the reaction name on the message at r. A reaction that is there already is success.
	React(ctx context.Context, r Ref, name string) error
	// Unreact takes the reaction name that React put on the message at r off it. A reaction that is not there is
	// success.
	Unreact(ctx context.Context, r Ref, name string) error

	// IsMember says whether user is in the conversation channel. It fails closed: an error is never
	// "yes".
	IsMember(ctx context.Context, channel, user string) (bool, error)
	// IsInAnyGroup says whether user is a member of at least one of groups, which are the platform's own group IDs (a
	// Slack user group's, for one). The membership is read at the call and never cached, so a person removed from a
	// group is refused at once, and the groups are read in order, stopping at the first that lists user. It fails
	// closed: an error is never "yes". A group the platform cannot read (it does not exist, the app may not read
	// groups, the platform does not answer in time) is false and the error: the first error ends the call, even if a
	// later group lists user. An error wraps ErrGroupsMisconfigured when the platform says the read can never succeed
	// until an operator fixes the configuration (a group that does not exist or is disabled, an app without the
	// permission to read groups, a workspace without groups): the caller checks it with errors.Is and does not retry.
	// Any other error (a timeout, a server error, a rate limit, a cancelled context) may pass on another try. Neither is
	// ever "yes". An empty groups, or an empty user, is false with no error and no call: what no groups
	// means is the caller's to decide, and it asks only when groups are configured.
	IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error)
	// GroupsReadable says whether Agent Kourier can read the members of every one of groups, the platform's own group IDs as
	// IsInAnyGroup takes them, without asking whose they are: it is the check that IsInAnyGroup would fail on, made before a
	// person presses. The groups are read at the call and never cached, in order, and the first that cannot be read ends the call
	// with a *GroupError that names it (no later group is read, so one failure hides the ones after it until it is mended). An
	// error wraps ErrGroupsMisconfigured when the platform says the read can never succeed until an operator fixes the
	// configuration, and then also ErrGroupNotFound for a group that does not exist or is disabled, and ErrMissingScope for an
	// app without the permission to read groups; a caller that wants the other causes (a workspace without groups, a token of a
	// type the read does not take) finds ErrGroupsMisconfigured and neither of those. Any other error (a timeout, a server
	// error, a rate limit, a cancelled context, a credential the platform rejects, which Classify says is FatalAuth) may pass on
	// another try, and wraps none of the three. An empty groups is nil with no call.
	GroupsReadable(ctx context.Context, groups []string) error
	// ChannelAccess says how Agent Kourier itself stands to the conversation channel, read at the call and never cached:
	// whether it is a member (Access.Member) and whether the conversation is archived (Access.Archived). It reads
	// nothing else and posts nothing, and it does not page through the members of the channel to answer, so its cost
	// does not grow with the channel. An error leaves the Access zero, which is not an answer: the caller treats an error
	// as "unknown", never as "not a member". An error wraps ErrChannelNotFound when the platform says there is no such
	// channel for Agent Kourier to see, and ErrMissingScope when the platform refuses the read for want of a permission
	// the app was installed without; the caller checks them with errors.Is and does not retry. Classify agrees: for
	// OpOther it says Permanent for an error that wraps either, with ReasonChannelGone for ErrChannelNotFound and the
	// platform's own code, when it has one, in Verdict.Code, so a caller that asks Classify instead of errors.Is does not retry them
	// either. A credential the platform rejects wraps neither: Classify(OpOther, err) says FatalAuth for it, and
	// ChannelAccess itself stops nothing, so the caller decides what a rejected credential means.
	// Any other error (a timeout, a server error, a rate limit, a cancelled context) may pass on another try and stays
	// unclassified: Classify says Transient for it, with RetryAfter for a rate limit. An empty channel is
	// ErrChannelNotFound with no call.
	ChannelAccess(ctx context.Context, channel string) (Access, error)
	// IsBot says whether user is a bot. It fails closed: a user it cannot read is a bot, with the error.
	IsBot(ctx context.Context, user string) (bool, error)
	// ResolveUser returns the user with the platform's ID id.
	ResolveUser(ctx context.Context, id string) (User, error)

	// Fits says whether p can be shown in one message, and is ErrTooLarge if it cannot. It is pure, and
	// is asked before a prompt is stored, so that a pause that cannot be shown is refused at once. A prompt
	// of tools is ErrTooLarge too when the text of one tool, cleaned, is longer than its place's limit
	// (Caps.Limit): the text of a tool awaiting approval is never shown cut. A platform that shows no tool
	// approvals gives ErrInvalid for a prompt of tools.
	Fits(p Prompt) error
	// Classify says what an error returned by one of the methods means for a retry.
	Classify(op Op, err error) Verdict
	// Capabilities says what the platform can do and how much it shows.
	Capabilities() Caps

	Permalinker
	ThreadReader
	BusyMarker
}

// Op is the kind of call an error came from, since what is permanent differs: a post to a channel that
// is gone will never work, and a reaction to a message that is gone will never work either, but for other
// reasons and other codes.
type Op int

// The operations Classify tells apart.
const (
	OpPost  Op = iota // Post, Find, Update, Stream and Ephemeral
	OpReact           // React and Unreact
	OpOther           // anything else
)

// Class is what an error means for a retry.
type Class int

// The classes.
const (
	// Transient may pass on another try: a rate limit, a server error, a timeout.
	Transient Class = iota
	// Permanent will never succeed for this call, however often it is tried.
	Permanent
	// FatalAuth is a credential the platform rejects. No call, and no restart, can succeed until it is
	// changed.
	FatalAuth
)

// Reason says why a Permanent error is: a closed set, so that it can label a metric.
type Reason string

// The reasons. Other is every reason not named, and the platform's own code goes in Verdict.Code.
const (
	ReasonChannelGone    Reason = "channel_gone"    // the conversation or message is gone, archived, or Agent Kourier is not in it
	ReasonTooLong        Reason = "too_long"        // the text is longer than the platform takes
	ReasonInvalidContent Reason = "invalid_content" // the platform refuses the content's form
	ReasonOther          Reason = "other"           // permanent, for a reason not named
	ReasonNone           Reason = ""                // not Permanent
)

// Verdict is the classification of an error.
type Verdict struct {
	Class  Class
	Reason Reason
	// Code is the platform's own code for the error ("channel_not_found" for Slack), for a log and not a
	// metric, or "".
	Code string
	// RetryAfter is, for a Transient error that is a rate limit, how long the platform asked the caller to wait: at
	// least a second, because a rate limit that names no wait is reported as one. It is 0 if the error is no rate
	// limit, so that RetryAfter > 0 says whether it is.
	RetryAfter time.Duration
}
