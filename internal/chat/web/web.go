// Package web is the web chat platform (ChatConnection spec.platform web). A connection takes signed HTTP requests on the broker's
// chat listener (chat.HTTPReceiver) and, when its profile sends replies, POSTs them to the one URL of its spec (spec.web.url).
//
// What a request and a reply are is the connection's profile's (spec.web.profile): a Profile verifies and decodes the requests, and
// encodes the replies. The platform does the rest, the same for every profile: it refuses a request the profile does not verify before
// anything of its body is decoded, hands the event to the Sink on Run's context and answers once the Sink has taken it (the turn runs
// after the answer), keeps the events of a connection in order, sends the replies to the fixed URL with a bounded client that follows
// no redirect, and says what their errors mean (Classify). A new integration is a package that implements Profile and passes
// chattest.ProfileConformance.
//
// The platform has no streams, steps, ephemeral messages, busy marks, thread reads, reactions, user groups or button presses: each
// method says so in the way its contract allows.
package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Name is the platform's name, as ChatConnection spec.platform and chat.Ref.Platform carry it.
const Name = v1alpha1.PlatformWeb

// The bounds of the platform. Each is a constant: no setting changes it.
const (
	// ackWait is how long the handler waits for the Sink to take an event before it answers 503, for the sender to send it again. It
	// counts from when the handler is called, inside the chat listener's own deadline for the handler (chatHandlerTimeout in
	// internal/broker), and covers the wait for the event before it in the connection's order too.
	ackWait = 2 * time.Second
	// busyRetryAfter is the Retry-After of a 503: how long a sender is asked to wait before it sends an event that was not taken again.
	busyRetryAfter = time.Second
	// replyTimeout bounds one reply's request as a whole: the dial, the wait for the answer and the reading of what is read of it. Past
	// it the reply fails as Transient, and the caller (the outbox, the live output) tries it again on its own schedule.
	replyTimeout = 10 * time.Second
	// maxRetryAfter is the longest wait that a receiver's Retry-After is taken for, so that a receiver cannot hold the live output of a
	// thread, which waits as it is asked, for longer.
	maxRetryAfter = time.Minute
	// maxAnswerRead is the most bytes of a receiver's answer that are read, so that the connection can be used again; the answer's
	// content is not used.
	maxAnswerRead = 4 << 10
)

// Profile is one integration that a web connection speaks (spec.web.profile). Its methods are safe for concurrent use and do no I/O:
// the platform reads the requests and sends the replies.
type Profile interface {
	// Name is the profile's name, as spec.web.profile holds it.
	Name() v1alpha1.WebProfile
	// Verify says whether a request is the sender's, from its headers and its body, raw and whole, at the time now. It decodes nothing
	// of the body but to check it. It returns nil, or an error that says why not in words of its own (it quotes neither the request nor
	// the secret), which the platform answers 401. A profile whose signature carries a time refuses one too far from now, so that a
	// captured request cannot be replayed later.
	Verify(h http.Header, body []byte, now time.Time) error
	// Decode reads a request that Verify passed into the event it carries. ok is false for a request that carries nothing to take, such
	// as a sender's ping: the platform answers it and hands nothing on. An error wrapping ErrTooLarge is a body over the profile's bound,
	// answered 413; any other error is a body that is not the profile's schema, answered 400. Neither quotes the body.
	Decode(h http.Header, body []byte) (ev Event, ok bool, err error)
	// Caps says what the platform shows of the profile's replies: their limits. A profile sets nothing else of chat.Caps, since the
	// platform has no streams, steps, ephemeral messages, busy marks or reactions, and New refuses one that does.
	Caps() chat.Caps
	// Encoder returns how the profile sends replies, or nil for a profile that sends none: Agent Kourier then posts nothing on the
	// connection (the agent replies itself), and Post, Update, Ephemeral and Fits say ErrNoReplies.
	Encoder() Encoder
}

// Event is what one request carries, as its profile decodes it: a message written in a conversation.
type Event struct {
	// Channel is the conversation, and Thread the message that opened the thread the event is in, or "" for an event that opens one.
	// Each has the profile's shape (v1alpha1.ChatPlatformOf), and neither holds ':'.
	Channel, Thread string
	// ID is the event's own: the same for every delivery of the same event, and different for every other, since the reply queue keeps
	// the IDs it has taken (chat.MessageID) and drops a delivery of one it has, across restarts. It is not empty and holds no ':'.
	ID string
	// User is who wrote it, and Text what they wrote, verbatim and untrusted.
	User, Text string
	// Mentioned says the event is addressed to Agent Kourier (chat.Inbound.Mentioned): one that is not, in a thread Agent Kourier has
	// not been asked into, starts nothing.
	Mentioned bool
}

// Encoder makes the requests that send a profile's replies. The platform sends each with POST to the connection's URL, which comes from
// its spec alone and never from a request.
type Encoder interface {
	// Encode returns the headers and the body of the request that sends r, signed at now. A reply the profile cannot carry is an error
	// wrapping chat.ErrInvalid, and one too large for it an error wrapping chat.ErrTooLarge.
	Encode(r Reply, now time.Time) (Request, error)
}

// ReplyKind is what a reply does.
type ReplyKind string

// The kinds of a reply.
const (
	ReplyPost   ReplyKind = "post"   // a new message, whose ID is the reply's
	ReplyUpdate ReplyKind = "update" // the content of the message with the reply's ID, replaced
)

// Reply is one message that Agent Kourier sends on a connection.
type Reply struct {
	Kind ReplyKind
	// Connection is the ChatConnection, "namespace/name"; Channel and Thread are where the message is; and ID is the message's: the
	// outbox's tag for a post, and the ID of the message it replaces for an update.
	Connection, Channel, Thread, ID string
	// Key is the request's idempotency key. It is the same on every try of a post, so that the receiver can drop a repeat (Find cannot
	// ask whether a post landed), and new for each update, which replaces content and is the same if applied twice.
	Key string
	// Message is what to say: valid (chat.Outgoing.Validate), with no prompt of tools, and untrusted text that the encoder makes inert
	// for its receiver.
	Message chat.Outgoing
}

// Request is a reply as its profile encodes it.
type Request struct {
	Header http.Header
	Body   []byte
}

var (
	// ErrTooLarge is wrapped by the error of a Profile's Decode for a body over the profile's bound.
	ErrTooLarge = errors.New("web: the body is larger than the profile takes")
	// ErrNoReplies is the error of Post, Update, Ephemeral and Fits on a connection whose profile sends no replies. It wraps
	// chat.ErrInvalid, so that a pause is refused as unusable, and no retry mends it.
	ErrNoReplies = fmt.Errorf("web: the profile sends no replies: %w", chat.ErrInvalid)
	// errNoEphemeral is what Ephemeral says on a profile that sends replies: there is no message to one person alone.
	errNoEphemeral = errors.New("web: the platform has no ephemeral messages")
	// errNotStreaming is what a StreamAt's methods say: there are no streams.
	errNotStreaming = fmt.Errorf("web: %w", chat.ErrNotStreaming)
)

// Config is what a web connection is built from.
type Config struct {
	// Connection is the ChatConnection, "namespace/name", as Refs and Targets carry it.
	Connection string
	// Profile is the connection's profile (spec.web.profile), made with the connection's credentials.
	Profile Profile
	// URL is where the replies go (spec.web.url): an absolute http or https URL, required for a profile that sends replies and refused
	// for one that does not.
	URL string
	// Transport makes the replies' requests; http.DefaultTransport if nil. The client around it is the platform's own, bounded by
	// replyTimeout, and follows no redirect, so that a receiver cannot send the signed body on to another address.
	Transport http.RoundTripper
	// Now is the clock of the signatures; time.Now if nil.
	Now    func() time.Time
	Logger *slog.Logger
}

// Platform is a web connection. It implements chat.Platform and chat.HTTPReceiver. Every method is safe for concurrent use.
type Platform struct {
	conn    string
	profile Profile
	enc     Encoder
	shapes  v1alpha1.ChatPlatform
	url     string
	client  *http.Client
	now     func() time.Time
	log     *slog.Logger
	ackWait time.Duration
	// run is the Run that takes requests, or nil: the handler answers 503 then, and the sender sends the request again.
	run atomic.Pointer[run]
	// order holds one event at a time to the Sink, which keeps the events of a channel in order (chat.Sink). It is one place for the
	// whole connection, which is coarser than the contract asks: the Sink takes an event in milliseconds, and a connection's senders
	// are few.
	order chan struct{}
	// afterLoad, if set, is called by the handler once it has loaded the Run to hand an event to: a test holds the handler there. It is
	// nil outside tests.
	afterLoad func()
}

var (
	_ chat.Platform     = (*Platform)(nil)
	_ chat.HTTPReceiver = (*Platform)(nil)
)

// New returns the platform of cfg, or an error for a config it cannot run with. The error holds no secret.
func New(cfg Config) (*Platform, error) {
	if cfg.Connection == "" {
		return nil, errors.New("web: no connection")
	}
	if cfg.Profile == nil {
		return nil, fmt.Errorf("web: connection %s: no profile", cfg.Connection)
	}
	name := cfg.Profile.Name()
	shapes, ok := v1alpha1.ChatPlatformOf(v1alpha1.ChatConnectionSpec{Platform: Name, Web: &v1alpha1.WebConnectionSpec{Profile: name}})
	if !ok {
		return nil, fmt.Errorf("web: connection %s: the profile %q is not one the API knows", cfg.Connection, name)
	}
	enc := cfg.Profile.Encoder()
	if err := checkCaps(cfg.Profile.Caps(), enc != nil); err != nil {
		return nil, fmt.Errorf("web: connection %s: profile %s: %w", cfg.Connection, name, err)
	}
	switch {
	case enc == nil && cfg.URL != "":
		return nil, fmt.Errorf("web: connection %s: the profile %s sends no replies, and takes no URL", cfg.Connection, name)
	case enc != nil:
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("web: connection %s: the URL of the replies is not an absolute http or https URL", cfg.Connection)
		}
	}
	client := &http.Client{
		Transport:     cfg.Transport,
		Timeout:       replyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Platform{
		conn: cfg.Connection, profile: cfg.Profile, enc: enc, shapes: shapes, url: cfg.URL, client: client,
		now: now, log: cmp.Or(cfg.Logger, slog.Default()), ackWait: ackWait, order: make(chan struct{}, 1),
	}, nil
}

// checkCaps says whether a profile's caps are ones the platform keeps: limits alone, and a limit that holds the truncation marker
// wherever text is shown.
func checkCaps(c chat.Caps, replies bool) error {
	switch {
	case c.Streams, c.Steps, c.Ephemeral, c.Busy, c.AckReaction != "":
		return errors.New("the caps name a feature the web platform does not have (streams, steps, ephemeral messages, busy marks or reactions)")
	case !replies:
		return nil
	case c.MaxText <= chat.Units(chat.Truncated):
		return fmt.Errorf("MaxText is %d, which cannot hold the truncation marker", c.MaxText)
	}
	for w, n := range c.Limits {
		if n <= chat.Units(chat.Truncated) {
			return fmt.Errorf("the limit of %s is %d, which cannot hold the truncation marker", w, n)
		}
	}
	return nil
}

// run is one Run, as the handler finds it. The handler calls the Sink under mu held for reading, and Run sets closed under mu held for
// writing once its context has ended, so that no event is handed to the Sink of a Run that has returned.
type run struct {
	sink chat.Sink
	// ctx is Run's: the Sink is called on a context of it, which carries what the broker put there (the telemetry the intake counts
	// through), and ends when Run's does.
	ctx    context.Context
	mu     sync.RWMutex
	closed bool // guarded by mu
}

// Run takes the requests of the connection to sink until ctx ends. The listener is the broker's, which routes to the handler of
// HTTPHandler, so Run opens nothing: it makes the handler hand events to sink, and a request that comes before or after it is answered
// 503.
func (p *Platform) Run(ctx context.Context, sink chat.Sink) error {
	r := &run{sink: sink, ctx: ctx}
	p.run.Store(r)
	p.log.InfoContext(ctx, "web: taking requests", "connection", p.conn, "profile", p.profile.Name())
	<-ctx.Done()
	p.run.CompareAndSwap(r, nil)
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return fmt.Errorf("web: %w", ctx.Err())
}

// Capabilities implements chat.Platform: the profile's.
func (p *Platform) Capabilities() chat.Caps { return p.profile.Caps() }

// RefAt implements chat.Platform.
func (p *Platform) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: Name, Conn: p.conn, Channel: channel, Thread: thread, ID: id}
}

// Stream implements chat.Platform: there are no streams.
func (p *Platform) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return nil, chat.ErrNoStreams
}

// StreamAt is a stream that is not streaming.
func (p *Platform) StreamAt(r chat.Ref) chat.Stream { return noStream{r} }

type noStream struct{ ref chat.Ref }

func (s noStream) Ref() chat.Ref                                            { return s.ref }
func (s noStream) Append(context.Context, string) error                     { return errNotStreaming }
func (s noStream) Step(context.Context, chat.Step) error                    { return errNotStreaming }
func (s noStream) Stop(context.Context, string, chat.Message, string) error { return errNotStreaming }

// Ephemeral implements chat.Platform: there are no messages to one person alone.
func (p *Platform) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	if p.enc == nil {
		return ErrNoReplies
	}
	return errNoEphemeral
}

// React implements chat.Platform: there are no reactions (Caps.AckReaction is empty).
func (p *Platform) React(context.Context, chat.Ref, string) error { return chat.ErrNoReactions }

// Unreact implements chat.Platform: there are no reactions.
func (p *Platform) Unreact(context.Context, chat.Ref, string) error { return chat.ErrNoReactions }

// MarkBusy implements chat.BusyMarker: there is no busy mark.
func (p *Platform) MarkBusy(context.Context, chat.Target, bool) error { return chat.ErrNoBusy }

// Permalink implements chat.Permalinker: the platform makes no links.
func (p *Platform) Permalink(chat.Ref) (string, bool) { return "", false }

// ThreadBetween implements chat.ThreadReader: the platform reads no thread.
func (p *Platform) ThreadBetween(context.Context, chat.Target, string, string, int) ([]chat.HistoryMessage, int, error) {
	return nil, 0, chat.ErrNoThreadReads
}

// ThreadFromStart implements chat.ThreadReader: the platform reads no thread.
func (p *Platform) ThreadFromStart(context.Context, chat.Target, string, int) ([]chat.HistoryMessage, int, error) {
	return nil, 0, chat.ErrNoThreadReads
}

// IsMember says no: what it answers is whether a person may press a button, and the platform has no presses.
func (p *Platform) IsMember(context.Context, string, string) (bool, error) { return false, nil }

// IsBot says no: an event is a person's, as its sender reports it, and the platform forwards no bot's message.
func (p *Platform) IsBot(context.Context, string) (bool, error) { return false, nil }

// ResolveUser returns the user by the ID alone: the sender reports no name or email.
func (p *Platform) ResolveUser(_ context.Context, id string) (chat.User, error) {
	return chat.User{ID: id, Name: id}, nil
}

// IsInAnyGroup says no, with the error of the first group: the platform has no user groups.
func (p *Platform) IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) {
	if len(groups) == 0 || user == "" {
		return false, nil
	}
	return false, p.GroupsReadable(ctx, groups)
}

// GroupsReadable says that the first group does not exist: the platform has no user groups.
func (p *Platform) GroupsReadable(ctx context.Context, groups []string) error {
	if len(groups) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return &chat.GroupError{Group: groups[0], Err: fmt.Errorf("web: no user groups: %w: %w", chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)}
}

// ChannelAccess has no platform to ask: a channel that has the profile's shape of a channel ID is one that Agent Kourier is in and that
// is not archived, and any other is not found.
func (p *Platform) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	if err := ctx.Err(); err != nil {
		return chat.Access{}, err
	}
	if !p.shapes.Channel.Matches(channel) {
		return chat.Access{}, fmt.Errorf("web: %w", chat.ErrChannelNotFound)
	}
	return chat.Access{Member: true}, nil
}
