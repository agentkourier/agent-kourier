// Package webhook is the webhook chat platform: a connection takes signed JSON webhooks (GitHub, Jira Cloud, Bitbucket Cloud,
// Linear, or any sender that signs its body with a shared secret or sends a fixed token) on the broker's chat listener
// (chat.HTTPReceiver), and maps each onto an event by JSON Pointers. It sends nothing: the agent replies on the source itself.
//
// A request goes through these steps, each with the answer, outcome and reason it gives when it ends there; no step runs after one
// that ends it:
//
//  1. verify the raw bytes against the connection's secret or token, and its body timestamp if it has one: 401, unverified
//     (unsigned, bad_signature, stale);
//  2. parse the body, one pass over it for every pointer the connection reads: 400, malformed (schema, depth, duplicate_key);
//  3. the channel, the first of its pointers that resolves, or the fixed value: 200, unmapped (no_channel) when none resolves or
//     its value is not a channel's shape;
//  4. the actor: 200, self, when any actor value that resolves is the connection's identity; 200, ignored (no_actor), when none
//     resolves and the connection keeps no such event;
//  5. the fields, then the thread from its field: 200, unmapped, when the thread's field resolves to no value that passes the id
//     check (bad_id) or does not resolve (no_thread);
//  6. the Sink, after the connection's events before it: 200, accepted, once it has taken the event; 503, not_running, withheld,
//     timeout or cancelled, with a Retry-After, when it did not.
//
// Every answer but the 401, the 400 and the 503 is 200: some senders count any other as a failed delivery. A request that cannot be
// read (400, malformed, unreadable) is refused before step 1. Each answer is counted once on the Config's Recorder, and every
// outcome and reason it can count exists at zero from when a Run starts. Each verified request is logged once, at INFO; an
// unverified one at DEBUG, and in a WARN that sums them for the connection at most once a minute, so that a scanner of a public
// URL cannot flood the log.
//
// The platform has no replies, streams, steps, ephemeral messages, busy marks, thread reads, reactions, user groups or button
// presses: each method says so in the way its contract allows.
package webhook

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// Name is the platform's name, as ChatConnection spec.platform and chat.Ref.Platform carry it.
const Name = "webhook"

// The bounds of the platform. Each is a constant: no setting changes it.
const (
	// ackWait is how long the handler waits for the Sink to take an event before it answers 503, for the sender to send it again. It
	// counts from when the handler is called, inside the chat listener's own deadline for the handler (chatHandlerTimeout in
	// internal/broker), and covers the wait for the event before it in the connection's order too.
	ackWait = 2 * time.Second
	// busyRetryAfter is the Retry-After of a 503: how long a sender is asked to wait before it sends an event that was not taken again.
	busyRetryAfter = time.Second
	// maxIDValue is the longest value of an id field, and of a delivery ID, in bytes: the prompt renderer's bound on a key.
	maxIDValue = 128
	// minTokenLen is the shortest token the token scheme takes, in bytes: a shorter one is refused when the platform is built.
	minTokenLen = 32
	// unverifiedLogEvery is the shortest time between two WARN lines that sum a connection's unverified requests.
	unverifiedLogEvery = time.Minute
)

// Scheme is how a connection checks that a request is its sender's.
type Scheme string

// The schemes.
const (
	// SchemeHMACSHA256 is the hex HMAC-SHA256 of the raw body under the connection's secret, in a header, after a prefix.
	SchemeHMACSHA256 Scheme = "hmac-sha256"
	// SchemeToken is a token in a header, after a prefix, equal to the connection's.
	SchemeToken Scheme = "token"
)

var (
	// ErrControlCharacter is wrapped by the error of New for a secret or a token that holds a control character, most often the
	// trailing newline that a file read into a Secret keeps. The value is never trimmed: an HMAC key that differs from what the
	// sender signs with by one byte refuses every delivery, and which byte the operator meant is not the platform's to guess.
	ErrControlCharacter = errors.New("webhook: the value contains a control character")
	// ErrTokenTooShort is wrapped by the error of New for a token of the token scheme shorter than minTokenLen.
	ErrTokenTooShort = fmt.Errorf("webhook: the token is shorter than %d bytes", minTokenLen)
	// ErrNoReplies is wrapped by the error of Post, Update, Ephemeral and Fits: the platform sends nothing. It wraps chat.ErrInvalid,
	// so that a pause is refused as unusable, and no retry mends it.
	ErrNoReplies = fmt.Errorf("webhook: the platform sends no replies: %w", chat.ErrInvalid)
	// errNotStreaming is what a StreamAt's methods say: there are no streams.
	errNotStreaming = fmt.Errorf("webhook: %w", chat.ErrNotStreaming)
)

// credentialHeaders are the request headers that a field or the delivery ID may never read, besides the verify header: a credential
// must not reach a regex, the prompt or a log line.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie"}

// Config is what a webhook connection is built from: ChatConnection spec.webhook and the credentials it names, mapped by the broker.
type Config struct {
	// Connection is the ChatConnection, "namespace/name", as Refs, Targets and the metric's label carry it.
	Connection string
	// Identity is the agent's own account on the source, which the actor values are compared with (ASCII letters folded) to drop the
	// agent's own events. Required unless Actor.None.
	Identity string
	Verify   Verify
	Channel  Channel
	Actor    Actor
	// Fields are the event's named values, in the order chat.WebhookEvent carries them.
	Fields []Field
	// Thread names the field whose value, after a prefix, is the event's thread. Nil, every event is a thread of its own.
	Thread *Thread
	// DeliveryHeader names the request header that holds the sender's ID of a delivery (X-GitHub-Delivery), which the log lines and
	// chat.WebhookEvent.Delivery carry; empty, none is read. It must not be a credential header.
	DeliveryHeader string
	// ValidChannel says whether a value is a webhook channel's shape, the API's, which the broker hands in so that the platform and
	// the Bindings hold one shape. Required. The platform refuses a value with ':' whatever it says, since chat.MessageID joins with it.
	ValidChannel func(string) bool
	// Recorder counts each answer (agentkourier_webhook_requests_total); nil, nothing is counted.
	Recorder Recorder
	// Now is the clock of the body timestamps and the WARN summary; time.Now if nil.
	Now func() time.Time
	// OnConnection, if set, is told true when a Run starts to take requests and false when it stops: the connection is up while a
	// Run takes them, as a Slack connection in HTTP mode is.
	OnConnection func(connected bool)
	Logger       *slog.Logger
}

// Verify is how a request is checked, on its raw bytes, before anything of it is parsed.
type Verify struct {
	Scheme Scheme
	// Header holds the signature or the token, after Prefix ("sha256=", "Bearer ").
	Header, Prefix string
	// Secret is the HMAC key of SchemeHMACSHA256, or the token of SchemeToken.
	Secret Credential
	// Timestamp, of SchemeHMACSHA256 alone, is a time in the body that must be within its window of now.
	Timestamp *Timestamp
}

// Credential is a value of a Secret, with where it came from, which an error names in its place.
type Credential struct {
	// Secret is the Secret, "namespace/name", and Key its key.
	Secret, Key string
	Value       secret.Value
}

// Timestamp is a time in the body.
type Timestamp struct {
	// From is the JSON Pointer to the time, an integer JSON number.
	From string
	// Unit is time.Second or time.Millisecond.
	Unit time.Duration
	// Window is how far from now the time may be, either way. Above zero.
	Window time.Duration
}

// Channel is where an event belongs: the first of From's pointers that resolves, or the fixed Value. Set one of them.
type Channel struct {
	From  []string
	Value string
}

// Actor is who caused an event.
type Actor struct {
	// From lists the pointers that can name who caused an event, each compared with the identity. Required unless None.
	From []string
	// None turns the self-drop off, for a source that has no actors.
	None bool
	// KeepMissing takes an event in which no actor pointer resolves; by default it is dropped (ignored, no_actor).
	KeepMissing bool
}

// Field is one named value of an event: the first of From's pointers into the body that resolves, or the first value of Header. Set
// one of them.
type Field struct {
	Name   string
	From   []string
	Header string
	// ID makes it an id field: a value that is not at most maxIDValue bytes of ASCII letters, digits and "_.-:/" is passed over for
	// the next, and a field none of whose values passes is Invalid.
	ID bool
}

// Thread names the field of an event's thread.
type Thread struct {
	// Field is an id field of the connection.
	Field string
	// Prefix comes before the field's value in the thread, such as "pr-".
	Prefix string
}

// Recorder counts the requests of one connection, by outcome and reason (telemetry.WebhookRequests).
type Recorder interface {
	// Init makes the series of outcome and reason exist, at zero if it did not.
	Init(outcome, reason string)
	// Count adds one to the series of outcome and reason.
	Count(outcome, reason string)
}

// field is a configured field, parsed.
type field struct {
	name   string
	header string
	from   []pointer
	id     bool
}

// Platform is a webhook connection. It implements chat.Platform and chat.HTTPReceiver. Every method is safe for concurrent use.
type Platform struct {
	conn     string
	identity string
	v        verifier
	channel  []pointer
	value    string // the fixed channel, when channel is empty
	actors   []pointer
	none     bool
	keep     bool
	fields   []field
	thread   int // the index in fields of the thread's field, or -1
	prefix   string
	ptrs     []pointer // every pointer the extractor reads
	delivery string
	valid    func(string) bool
	rec      Recorder
	now      func() time.Time
	onConn   func(connected bool)
	log      *slog.Logger
	ackWait  time.Duration
	// run is the Run that takes requests, or nil: the handler answers 503 then, and the sender sends the request again.
	run atomic.Pointer[run]
	// order holds one event at a time to the Sink, which keeps the events of a channel in order (chat.Sink). It is one place for the
	// whole connection, which is coarser than the contract asks: the Sink takes an event in milliseconds, and a connection's senders
	// are few.
	order chan struct{}
	// unverified sums the unverified requests for the WARN line.
	unverified unverifiedLog
	// afterLoad, if set, is called by the handler once it has loaded the Run to hand an event to: a test holds the handler there. It is
	// nil outside tests.
	afterLoad func()
}

var (
	_ chat.Platform     = (*Platform)(nil)
	_ chat.HTTPReceiver = (*Platform)(nil)
)

// New returns the platform of cfg, or an error for a config it cannot run with: one the API refuses in a way that would break the
// platform, a missing credential, a secret or token with a control character (ErrControlCharacter) and a token shorter than
// minTokenLen (ErrTokenTooShort). An error about a credential names its Secret and key, and none holds a credential's value.
func New(cfg Config) (*Platform, error) {
	if cfg.Connection == "" {
		return nil, errors.New("webhook: no connection")
	}
	fail := func(format string, args ...any) (*Platform, error) {
		return nil, fmt.Errorf("webhook: connection %s: %w", cfg.Connection, fmt.Errorf(format, args...))
	}
	p := &Platform{conn: cfg.Connection, identity: cfg.Identity, value: cfg.Channel.Value, none: cfg.Actor.None,
		keep: cfg.Actor.KeepMissing, thread: -1, delivery: cfg.DeliveryHeader, valid: cfg.ValidChannel, rec: cfg.Recorder,
		now: cfg.Now, onConn: cfg.OnConnection, log: cmp.Or(cfg.Logger, slog.Default()), ackWait: ackWait, order: make(chan struct{}, 1)}
	if p.rec == nil {
		p.rec = noRecorder{}
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.valid == nil {
		return fail("no channel shape")
	}
	vf := cfg.Verify
	if vf.Header == "" {
		return fail("no verify header")
	}
	p.v = verifier{scheme: vf.Scheme, header: vf.Header, prefix: vf.Prefix}
	if err := checkCredential(vf.Secret); err != nil {
		return fail("%w", err)
	}
	switch vf.Scheme {
	case SchemeHMACSHA256:
		p.v.key = []byte(vf.Secret.Value.Reveal())
	case SchemeToken:
		tok := vf.Secret.Value.Reveal()
		if len(tok) < minTokenLen {
			return fail("secret %s key %q: %w", vf.Secret.Secret, vf.Secret.Key, ErrTokenTooShort)
		}
		p.v.digest = sha256.Sum256([]byte(tok))
	default:
		return fail("the verify scheme %q is not one the platform knows", vf.Scheme)
	}
	if ts := vf.Timestamp; ts != nil {
		ptr, err := parsePointer(ts.From)
		switch {
		case vf.Scheme != SchemeHMACSHA256:
			return fail("a body timestamp is taken by the %s scheme alone", SchemeHMACSHA256)
		case err != nil:
			return fail("the timestamp: %w", err)
		case ts.Unit != time.Second && ts.Unit != time.Millisecond:
			return fail("the timestamp's unit is %s, not a second or a millisecond", ts.Unit)
		case ts.Window <= 0:
			return fail("the timestamp's window is %s, not above zero", ts.Window)
		}
		p.v.ts = &timestamp{ptr: ptr, unit: ts.Unit, window: ts.Window}
	}
	parse := func(what string, from []string) ([]pointer, error) {
		var out []pointer
		for _, s := range from {
			ptr, err := parsePointer(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			out = append(out, ptr)
			p.ptrs = append(p.ptrs, ptr)
		}
		return out, nil
	}
	var err error
	if p.channel, err = parse("the channel", cfg.Channel.From); err != nil {
		return fail("%w", err)
	}
	switch {
	case (len(p.channel) == 0) == (p.value == ""):
		return fail("the channel has both pointers and a value, or neither")
	case p.value != "" && !p.channelShape(p.value):
		return fail("the channel's value is not a channel's shape")
	}
	if p.actors, err = parse("the actor", cfg.Actor.From); err != nil {
		return fail("%w", err)
	}
	switch {
	case p.none && len(p.actors) > 0:
		return fail("the actor has pointers, and none turns the self-drop off")
	case !p.none && (len(p.actors) == 0 || p.identity == ""):
		return fail("an identity and an actor pointer are required unless the actor is none")
	}
	if p.credentialHeader(p.delivery) {
		return fail("the delivery header %s is a credential header", p.delivery)
	}
	names := map[string]bool{}
	for _, f := range cfg.Fields {
		switch {
		case f.Name == "":
			return fail("a field has no name")
		case names[f.Name]:
			return fail("the field %s is named twice", f.Name)
		case (len(f.From) == 0) == (f.Header == ""):
			return fail("the field %s has both pointers and a header, or neither", f.Name)
		case p.credentialHeader(f.Header):
			return fail("the field %s reads the credential header %s", f.Name, f.Header)
		}
		names[f.Name] = true
		ptrs, err := parse("the field "+f.Name, f.From)
		if err != nil {
			return fail("%w", err)
		}
		p.fields = append(p.fields, field{name: f.Name, header: f.Header, from: ptrs, id: f.ID})
	}
	if th := cfg.Thread; th != nil {
		p.thread = -1
		for i, f := range p.fields {
			if f.name == th.Field {
				p.thread = i
			}
		}
		switch {
		case p.thread < 0:
			return fail("the thread's field %s is not a field of the connection", th.Field)
		case !p.fields[p.thread].id:
			return fail("the thread's field %s is not an id field", th.Field)
		case strings.Contains(th.Prefix, ":"):
			return fail("the thread's prefix holds ':'")
		}
		p.prefix = th.Prefix
	}
	return p, nil
}

// checkCredential returns an error, naming the Secret and key and never the value, for a credential that cannot be used: it is empty,
// or holds a control character.
func checkCredential(c Credential) error {
	switch s := c.Value.Reveal(); {
	case s == "":
		return fmt.Errorf("secret %s key %q: %w", c.Secret, c.Key, secret.ErrEmptyValue)
	case strings.ContainsFunc(s, unicode.IsControl):
		return fmt.Errorf("secret %s key %q: %w", c.Secret, c.Key, ErrControlCharacter)
	}
	return nil
}

// credentialHeader says whether a field or the delivery ID would read a credential from the header name.
func (p *Platform) credentialHeader(name string) bool {
	return name != "" && slices.ContainsFunc(append([]string{p.v.header}, credentialHeaders...), func(c string) bool {
		return strings.EqualFold(name, c)
	})
}

// channelShape says whether s can be an event's channel: the API's shape, and no ':'.
func (p *Platform) channelShape(s string) bool { return !strings.Contains(s, ":") && p.valid(s) }

// noRecorder is the Recorder of a Config that has none: it counts nothing.
type noRecorder struct{}

func (noRecorder) Init(string, string)  {}
func (noRecorder) Count(string, string) {}

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
// HTTPHandler, so Run opens nothing: it makes every series of the Recorder exist, makes the handler hand events to sink, and tells
// Config.OnConnection that the connection is up; a request that comes before or after it is answered 503. Once ctx ends it tells
// OnConnection the connection is down.
func (p *Platform) Run(ctx context.Context, sink chat.Sink) error {
	for _, s := range requestSeries {
		p.rec.Init(s.outcome, s.reason)
	}
	r := &run{sink: sink, ctx: ctx}
	p.run.Store(r)
	if p.onConn != nil {
		p.onConn(true)
	}
	p.log.InfoContext(ctx, "webhook: taking requests", "connection", p.conn)
	<-ctx.Done()
	p.run.CompareAndSwap(r, nil)
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	if p.onConn != nil {
		p.onConn(false)
	}
	p.log.InfoContext(context.WithoutCancel(ctx), "webhook: stopped taking requests", "connection", p.conn)
	return fmt.Errorf("webhook: %w", ctx.Err())
}

// Capabilities implements chat.Platform: none, since the platform sends nothing.
func (p *Platform) Capabilities() chat.Caps { return chat.Caps{} }

// RefAt implements chat.Platform.
func (p *Platform) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: Name, Conn: p.conn, Channel: channel, Thread: thread, ID: id}
}

// Post implements chat.Platform: the platform sends nothing.
func (p *Platform) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	return chat.Ref{}, ErrNoReplies
}

// Update implements chat.Platform: the platform sends nothing.
func (p *Platform) Update(context.Context, chat.Ref, chat.Outgoing) error { return ErrNoReplies }

// Find implements chat.Platform: the platform posts nothing, so there is nothing to find.
func (p *Platform) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	return chat.Ref{}, false, nil
}

// Fits implements chat.Platform: no prompt can be shown, since the platform sends nothing.
func (p *Platform) Fits(chat.Prompt) error { return ErrNoReplies }

// Ephemeral implements chat.Platform: the platform sends nothing, and has no messages to one person alone.
func (p *Platform) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	return fmt.Errorf("%w: %w", ErrNoReplies, chat.ErrNoEphemeral)
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

// IsBot says no: an event is whoever caused it, as its sender reports it, and the agent's own are dropped by the identity.
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
	return &chat.GroupError{Group: groups[0], Err: fmt.Errorf("webhook: no user groups: %w: %w", chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)}
}

// ChannelAccess has no platform to ask: a channel of the webhook shape is one that Agent Kourier is in and that is not archived, and
// any other is not found.
func (p *Platform) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	if err := ctx.Err(); err != nil {
		return chat.Access{}, err
	}
	if channel == "" || !p.channelShape(channel) {
		return chat.Access{}, fmt.Errorf("webhook: %w", chat.ErrChannelNotFound)
	}
	return chat.Access{Member: true}, nil
}

// Classify says what an error of the platform means for a retry: every error it returns but a cancelled context will not pass on
// another try.
func (p *Platform) Classify(_ chat.Op, err error) chat.Verdict {
	switch {
	case errors.Is(err, ErrNoReplies):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "no_replies"}
	case errors.Is(err, chat.ErrChannelNotFound):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "channel_not_found"}
	case errors.Is(err, chat.ErrNoReactions), errors.Is(err, chat.ErrNoEphemeral), errors.Is(err, chat.ErrNotStreaming),
		errors.Is(err, chat.ErrNoStreams), errors.Is(err, chat.ErrNoBusy), errors.Is(err, chat.ErrGroupsMisconfigured):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther}
	}
	return chat.Verdict{Class: chat.Transient}
}
