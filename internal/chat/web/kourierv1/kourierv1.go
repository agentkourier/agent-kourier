// Package kourierv1 is the kourier-v1 profile of the web platform (ChatConnection spec.web.profile kourier-v1): Agent Kourier's own
// signed JSON contract. A sender POSTs one message (Inbound) to the connection's route on the chat listener, signed with the
// connection's signingSecret, and Agent Kourier POSTs each reply (Outbound), signed the same way, to the connection's spec.web.url.
//
// A request is signed with two headers: HeaderTimestamp, the Unix seconds of the signing, and HeaderSignature, "v1=" and the hex
// HMAC-SHA256 under the secret of "v1:", the timestamp, ":" and the body. A request whose timestamp is more than maxSkew from the
// receiver's clock is refused, and a delivery of a message whose ID was taken already is dropped by the reply queue.
//
// A reply's text is inert for a receiver that keeps the contract: every field of the message is plain text, cleaned and cut to its
// limit, but a body part's markdown, which is CommonMark in which every character that could open a link, an image, an autolink, raw
// HTML or a mention is escaped outside code, and code, in blocks and spans that Agent Kourier writes itself, holds the agent's text as
// it is (internal/chat/commonmark). The profile has no buttons, presses or reactions: a Binding on it takes no questions, tool approvals
// or feedback (v1alpha1.ChatPlatformOf).
package kourierv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// The bounds of the profile. Each is a constant: no setting changes it.
const (
	// maxSkew is how far a request's timestamp may be from the receiver's clock, either way. A request signed further off is refused
	// (401), so that a captured request cannot be replayed later than this; within it, the repeat of a message is dropped by its ID.
	maxSkew = 5 * time.Minute
	// maxBody is the most bytes of a request's body: a longer one is refused (413). It bounds the text of a message, which becomes the
	// agent's prompt.
	maxBody = 64 << 10
	// maxText is the most UTF-16 units of text in each field of a reply (chat.Caps.MaxText): a longer one is cut, and ends in
	// chat.Truncated. A body's markdown is counted as it is sent, its escapes and fences with it.
	maxText = 40000
	// maxReplyBody is the most bytes of a reply's body. A reply past it is refused as too large, which no field's cut can cause alone:
	// a prompt of many questions, each within maxText, can.
	maxReplyBody = 1 << 20
)

// The versions of the wire.
const (
	// InboundVersion is the version of the schema a sender POSTs (Inbound.V). A body of another is refused.
	InboundVersion = 1
	// OutboundVersion is the version of a reply's envelope (Outbound.V). The message in it has its own (chat.OutgoingVersion).
	OutboundVersion = 1
)

// The headers of the wire. The two of the signature are on every request, both ways.
const (
	// HeaderTimestamp is the time the sender signed the request, in Unix seconds.
	HeaderTimestamp = "X-Kourier-Timestamp"
	// HeaderSignature is "v1=" and the hex HMAC-SHA256, under the connection's signing secret, of "v1:", the timestamp, ":" and the body.
	HeaderSignature = "X-Kourier-Signature"
	// HeaderIdempotencyKey is on every reply: the same on every try of a post, so that the receiver can drop a repeat, and new for each
	// update (web.Reply.Key).
	HeaderIdempotencyKey = "Idempotency-Key"
)

// Inbound is the one schema a sender POSTs, version InboundVersion. It is decoded strictly: a field it does not have, a second
// document, or a version other than InboundVersion is refused.
type Inbound struct {
	V int `json:"v"`
	// Channel is the conversation, which the Binding's spec.chat.channel names. It has the kourier-v1 shape of every ID below.
	Channel string `json:"channel"`
	// Thread is the ID of the message that opened the thread, for a reply in one, and empty for a message that opens one: the message's
	// own ID is then the thread's. A thread that is the message's own ID is refused.
	Thread string `json:"thread,omitempty"`
	// ID is the message's own, which the sender sends again unchanged with every delivery of the message: a delivery of an ID that was
	// taken is dropped. ID, Thread and User have the kourier-v1 shape of an ID (letters, digits, '.', '_' and '-', starting with a
	// letter or a digit, at most 64 bytes).
	ID   string `json:"id"`
	User string `json:"user"`
	// Text is what the person wrote, untrusted.
	Text string `json:"text"`
	// Mention says a reply in a thread is addressed to Agent Kourier. A message that opens a thread is, always; a reply is not unless it
	// says so, and on a Binding with chat.threadReplies mention only one that says so is taken.
	Mention bool `json:"mention,omitempty"`
}

// Outbound is the envelope of every reply, version OutboundVersion: where the message goes, its ID, and the message. Every text
// field of Message is plain text, to be shown as the characters it holds, but a body part's markdown, which is CommonMark to render
// (see the package comment).
type Outbound struct {
	V int `json:"v"`
	// Kind is post, a new message with the ID, or update, the new content of the message with the ID.
	Kind       web.ReplyKind `json:"kind"`
	Connection string        `json:"connection"`
	Channel    string        `json:"channel"`
	Thread     string        `json:"thread,omitempty"`
	ID         string        `json:"id"`
	Message    chat.Outgoing `json:"message"`
}

// Profile is the kourier-v1 profile of one connection, made with its signing secret. It implements web.Profile and web.Encoder.
type Profile struct {
	key    secret.Value
	shapes v1alpha1.ChatPlatform
	// ids is the shape of a message's ID, and of the thread's: the kourier-v1 opaque ID, which is the shape of a user ID too.
	ids v1alpha1.IDShape
}

var (
	_ web.Profile = (*Profile)(nil)
	_ web.Encoder = (*Profile)(nil)
)

// New returns the profile that checks and signs with signingSecret (the credentials Secret's signingSecret), and holds what it decodes
// to the shapes of platform, what the API says the connection's platform takes (v1alpha1.ChatPlatformOf of its spec). It returns an
// error for no secret, or a platform that takes no channel or no user ID.
func New(signingSecret secret.Value, platform v1alpha1.ChatPlatform) (*Profile, error) {
	if signingSecret.IsEmpty() {
		return nil, errors.New("kourier-v1: no signing secret")
	}
	if !platform.Channel.Taken() || !platform.User.Taken() {
		return nil, fmt.Errorf("kourier-v1: the platform %q takes no channel or no user ID", platform.Name)
	}
	return &Profile{key: signingSecret, shapes: platform, ids: platform.User}, nil
}

// Name implements web.Profile.
func (p *Profile) Name() v1alpha1.WebProfile { return v1alpha1.WebProfileKourierV1 }

// Caps implements web.Profile: every place shows maxText units.
func (p *Profile) Caps() chat.Caps { return chat.Caps{MaxText: maxText} }

// Encoder implements web.Profile: the profile sends replies.
func (p *Profile) Encoder() web.Encoder { return p }

// Verify implements web.Profile: the request is signed with the connection's secret within maxSkew of now.
func (p *Profile) Verify(h http.Header, body []byte, now time.Time) error {
	return VerifySignature([]byte(p.key.Reveal()), now, h.Get(HeaderTimestamp), h.Get(HeaderSignature), body)
}

// errSchema is the error of a body that is not the schema. It quotes nothing of the body.
var errSchema = errors.New("kourier-v1: the body is not the inbound schema")

// Decode implements web.Profile: the body is one Inbound, which is always a message to take.
func (p *Profile) Decode(_ http.Header, body []byte) (web.Event, bool, error) {
	if len(body) > maxBody {
		return web.Event{}, false, fmt.Errorf("kourier-v1: the body is %d bytes, over the %d byte bound: %w", len(body), maxBody, web.ErrTooLarge)
	}
	var in Inbound
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return web.Event{}, false, errSchema
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return web.Event{}, false, fmt.Errorf("%w: something follows the message", errSchema)
	}
	switch {
	case in.V != InboundVersion:
		return web.Event{}, false, fmt.Errorf("%w: the version is %d, want %d", errSchema, in.V, InboundVersion)
	case !p.shapes.Channel.Matches(in.Channel):
		return web.Event{}, false, fmt.Errorf("%w: the channel is not a kourier-v1 channel ID", errSchema)
	case !p.ids.Matches(in.ID), in.Thread != "" && !p.ids.Matches(in.Thread):
		return web.Event{}, false, fmt.Errorf("%w: the id or the thread is not a kourier-v1 ID", errSchema)
	case in.Thread == in.ID:
		return web.Event{}, false, fmt.Errorf("%w: the thread is the message's own id: a message that opens a thread names none", errSchema)
	case !p.shapes.User.Matches(in.User):
		return web.Event{}, false, fmt.Errorf("%w: the user is not a kourier-v1 user ID", errSchema)
	}
	return web.Event{Channel: in.Channel, Thread: in.Thread, ID: in.ID, User: in.User, Text: in.Text, Mentioned: in.Thread == "" || in.Mention}, true, nil
}

// Encode implements web.Encoder: the envelope of r, its message made inert (inert), as JSON, signed at now.
func (p *Profile) Encode(r web.Reply, now time.Time) (web.Request, error) {
	env := Outbound{V: OutboundVersion, Kind: r.Kind, Connection: r.Connection, Channel: r.Channel, Thread: r.Thread, ID: r.ID, Message: p.inert(r.Message)}
	body, err := json.Marshal(env)
	if err != nil {
		return web.Request{}, fmt.Errorf("kourier-v1: %w: %w", chat.ErrInvalid, err)
	}
	if len(body) > maxReplyBody {
		return web.Request{}, fmt.Errorf("kourier-v1: the reply is %d bytes, over the %d byte bound: %w", len(body), maxReplyBody, chat.ErrTooLarge)
	}
	ts, sig := Sign([]byte(p.key.Reveal()), now, body)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, sig)
	h.Set(HeaderIdempotencyKey, r.Key)
	return web.Request{Header: h, Body: body}, nil
}
