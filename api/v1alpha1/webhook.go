package v1alpha1

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Enumerations of a webhook connection.
type (
	// WebhookScheme is how a webhook connection checks that a request is its sender's.
	WebhookScheme string
	// WebhookFieldKind says what a field of a webhook event may be used for. The platform that decodes an event and the trigger
	// intake that renders its prompt share it.
	WebhookFieldKind string
	// WebhookWhenMissing says what happens to an event in which no actor pointer resolves.
	WebhookWhenMissing string
	// WebhookTimeUnit is the unit of a timestamp in a webhook's body.
	WebhookTimeUnit string
)

// Values of the webhook enumerations.
const (
	// WebhookHMACSHA256 is the hex HMAC-SHA256 of the raw body under the credentials Secret's webhookSecret, in a header, after a
	// prefix.
	WebhookHMACSHA256 WebhookScheme = "hmac-sha256"
	// WebhookToken is a token in a header, after a prefix, equal to the credentials Secret's token.
	WebhookToken WebhookScheme = "token"

	// WebhookKindText is a field of free text: it reaches the regexes and the untrusted block, and nothing else. It is the default.
	WebhookKindText WebhookFieldKind = "text"
	// WebhookKindID is a field of an identifier: the thread and the prompt template may read it.
	WebhookKindID WebhookFieldKind = "id"

	// WebhookMissingDrop drops an event in which no actor pointer resolves, as the agent's own may be one. It is the default.
	WebhookMissingDrop WebhookWhenMissing = "drop"
	// WebhookMissingKeep keeps such an event.
	WebhookMissingKeep WebhookWhenMissing = "keep"

	// WebhookUnitMillis reads a body timestamp as milliseconds since the Unix epoch.
	WebhookUnitMillis WebhookTimeUnit = "ms"
	// WebhookUnitSeconds reads a body timestamp as seconds since the Unix epoch.
	WebhookUnitSeconds WebhookTimeUnit = "s"

	// TriggerWebhookEvent is a trigger on an event of a webhook connection: it matches the connection's named fields. It is the only
	// trigger a webhook connection's Bindings take, and no other platform's take it.
	TriggerWebhookEvent TriggerType = "webhookEvent"
)

// The bounds of a webhook connection's spec. Each is also in the schema, as a maxItems, a maxLength or a rule, so that the apiserver
// refuses what the broker refuses; they count bytes, as Validate does, and the schema characters, so the schema is never the stricter.
// Each is set far above the recipes, which name at most 8 fields, 3 pointers in a list and pointers of under 40 bytes.
const (
	// MaxWebhookFields is the most fields a connection names.
	MaxWebhookFields = 32
	// MaxWebhookFrom is the most pointers of a list: a field's first-of list, the channel's, or the actor's.
	MaxWebhookFrom = 8
	// MaxWebhookPointerLen is the longest pointer, in bytes.
	MaxWebhookPointerLen = 256
	// MaxWebhookPrefixLen is the longest prefix of the verifier's header, or of a thread.
	MaxWebhookPrefixLen = 32
	// MaxWebhookIdentityLen is the longest identity. An Atlassian account ID is documented as at most 128 characters (unverified:
	// not readable in this repository).
	MaxWebhookIdentityLen = 128
	// MaxWebhookTimestampWindow is the widest window a body timestamp may be given, either way of now.
	MaxWebhookTimestampWindow = 10 * time.Minute
	// maxWebhookHeaderNameLen is the longest header name the connection names: the verifier's, a field's, the delivery's.
	maxWebhookHeaderNameLen = 64
	// maxWebhookFieldNameLen is the longest name of a field, as a mention name and an extract name are bounded.
	maxWebhookFieldNameLen = 63
	// maxWebhookChannelLen is the longest webhook channel, fixed or a Binding's: an owner/repo of GitHub is at most 140 bytes
	// (39 + 1 + 100, GitHub's own rules, unverified here).
	maxWebhookChannelLen = 140
)

// The shapes of a webhook connection's values. Each literal must equal the Pattern marker or the CEL rule that copies it
// (TestSchemaPatternsEqualTheGoLiterals, TestCELLiteralsEqualTheBrokersConstants).
var (
	// webhookChannel is a webhook channel: letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between
	// parts, as an owner/repo or a project key is. ':' is left out because chat.MessageID joins with it.
	webhookChannel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9._-]+)*$`)
	// webhookIdentity is an identity: a login, an app's login with [bot], an Atlassian account ID with ':', a UUID, or a Bitbucket
	// account's UUID in braces.
	webhookIdentity = regexp.MustCompile(`^[A-Za-z0-9_.:@{}\[\]/-]+$`)
	// webhookFieldName is a field's name: the untrusted block drops any other, so '_' is left out.
	webhookFieldName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	// webhookPointer is a JSON Pointer (RFC 6901) that is not the root: a leading '/', and '~' only as ~0 or ~1.
	webhookPointer = regexp.MustCompile(`^/(?:[^~]|~[01])*$`)
	// httpHeaderName is an HTTP header name: RFC 9110's token. \x60 is a backtick, which a marker cannot hold as itself.
	httpHeaderName = regexp.MustCompile(`^[-!#$%&'*+.^_|~0-9A-Za-z\x60]+$`)
	// webhookPrefix is the prefix of the verifier's header: printable ASCII, as a header value holds no other.
	webhookPrefix = regexp.MustCompile(`^[ -~]*$`)
	// webhookThreadPrefix is the prefix of a thread: the characters of a channel's part.
	webhookThreadPrefix = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
)

// webhookReservedNames are the field names that the prompt view gives a meaning already.
var webhookReservedNames = []string{extractKey, extractState}

// credentialHeaders are the headers that a field or the delivery may never read, besides verify.header: a credential must not reach
// a regex, the prompt, a log line or a dry run. Each is in lower case, as the rule that copies them compares lowerAscii.
var credentialHeaders = []string{"authorization", "proxy-authorization", "cookie"}

// The CEL rules below copy rules of Validate and refuse nothing that Validate accepts. A list or a field that Validate reads as unset
// when it is empty counts as unset here too. (Not in the doc comments, which the reference pages print.)

// WebhookSpec maps a signed JSON webhook onto an event: the account the agent works as, how a delivery is verified, its channel, its
// actor, its thread, its named fields, and the header that names a delivery. Each value is read with a JSON Pointer (RFC 6901), or a
// first-of list of them, where the first that resolves wins.
//
// +kubebuilder:validation:XValidation:rule=`(has(self.actor.none) && self.actor.none) || (has(self.identity) && self.identity != "")`,message="is required unless actor.none is true: the agent's own events are dropped by it",fieldPath=".identity"
// +kubebuilder:validation:XValidation:rule=`!(has(self.actor.none) && self.actor.none) || !has(self.identity) || self.identity == ""`,message="is set, but actor.none turns the self-drop off, which alone reads it",fieldPath=".identity"
// +kubebuilder:validation:XValidation:rule="!has(self.thread) || (has(self.fields) && self.fields.exists(f, f.name == self.thread.field && has(f.kind) && f.kind == 'id'))",message="must be the name of an id field of fields",fieldPath=".thread.field"
// +kubebuilder:validation:XValidation:rule="!has(self.fields) || self.fields.all(f, !has(f.header) || f.header.lowerAscii() != self.verify.header.lowerAscii())",message="a field reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)",fieldPath=".fields"
// +kubebuilder:validation:XValidation:rule="!has(self.delivery) || self.delivery.header.lowerAscii() != self.verify.header.lowerAscii()",message="reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)",fieldPath=".delivery.header"
type WebhookSpec struct {
	// Identity is the account the agent works as on the source, as the source names it in the actor pointers, such as
	// pr-reviewer[bot] or an Atlassian account ID: letters, digits and _.:@{}[]/-, at most 128 bytes. An event that it caused is
	// dropped. Required unless actor.none is true, which refuses it.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9_.:@{}\[\]/-]+)?$`
	// +optional
	Identity string `json:"identity,omitempty"`
	// Verify is how a delivery is checked, on its raw bytes, before anything of it is read.
	Verify WebhookVerify `json:"verify"`
	// Channel is where an event belongs: the Binding of the connection whose spec.chat.channel is the value takes it.
	Channel WebhookChannel `json:"channel"`
	// Actor is who caused an event, by which the agent's own events are dropped.
	Actor WebhookActor `json:"actor"`
	// Thread names the field whose value, after prefix, is the event's thread. Left out, every event is a thread of its own.
	// +optional
	Thread *WebhookThread `json:"thread,omitempty"`
	// Fields are the event's named values, in the order the agent reads them. At most 32, each named once.
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Fields []WebhookField `json:"fields,omitempty"`
	// Delivery names the header that carries the source's ID of a delivery, which the log line of each answered delivery shows.
	// Left out, the line shows none.
	// +optional
	Delivery *WebhookDelivery `json:"delivery,omitempty"`
}

// WebhookVerify is how a request is verified, on its raw bytes, before anything of it is parsed.
//
// +kubebuilder:validation:XValidation:rule="!has(self.timestamp) || self.scheme == 'hmac-sha256'",message="is taken by the hmac-sha256 scheme alone: the time is read after the signature passes",fieldPath=".timestamp"
type WebhookVerify struct {
	// Scheme is hmac-sha256, the hex HMAC-SHA256 of the body under the credentials Secret's webhookSecret, or token, the credentials
	// Secret's token itself.
	// +kubebuilder:validation:Enum=hmac-sha256;token
	Scheme WebhookScheme `json:"scheme"`
	// Header holds the signature or the token: an HTTP header name of at most 64 bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[-!#$%&'*+.^_|~0-9A-Za-z\x60]+$`
	Header string `json:"header"`
	// Prefix comes before the signature or the token in the header, such as "sha256=" or "Bearer ": printable ASCII, at most 32
	// bytes.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[ -~]*$`
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// Timestamp, of the hmac-sha256 scheme alone, is a time in the body that must be within window of now, either way. It is read
	// after the signature passes.
	// +optional
	Timestamp *WebhookTimestamp `json:"timestamp,omitempty"`
}

// WebhookTimestamp is a time in a webhook's body.
type WebhookTimestamp struct {
	// From is the pointer to the time, a JSON number.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self.matches('^/(?:[^~]|~[01])*$')",message="must be a JSON Pointer that is not the root"
	From string `json:"from"`
	// Unit is ms or s.
	// +kubebuilder:validation:Enum=ms;s
	Unit WebhookTimeUnit `json:"unit"`
	// Window is how far from now the time may be, either way: above 0 and at most 10m.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^([-+]?0|[-+]?([0-9]*\.?[0-9]*(ns|us|µs|μs|ms|s|m|h))+)$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('10m')",message="is out of range, must be above 0s and at most 10m"
	Window metav1.Duration `json:"window"`
}

// WebhookChannel is where an event's channel comes from: a first-of list of pointers, or a fixed value. Set one of from and value.
//
// +kubebuilder:validation:XValidation:rule=`(has(self.from) && size(self.from) > 0) != (has(self.value) && self.value != "")`,message="set from or value, and not both"
type WebhookChannel struct {
	// From is a first-of list of pointers to the channel. At most 8, each at most 256 bytes.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.matches('^/(?:[^~]|~[01])*$'))",message="must be JSON Pointers that are not the root"
	// +optional
	From []string `json:"from,omitempty"`
	// Value is a fixed channel, for a source that has one: letters, digits, '.', '_' and '-', starting with a letter or a digit,
	// with '/' between parts, at most 140 bytes.
	// +kubebuilder:validation:MaxLength=140
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9._-]+)*)?$`
	// +optional
	Value string `json:"value,omitempty"`
}

// WebhookActor is who caused an event: the event is dropped as the agent's own when any pointer that resolves equals the
// connection's identity, compared ASCII case-insensitive.
//
// +kubebuilder:validation:XValidation:rule="(has(self.none) && self.none) || (has(self.from) && size(self.from) > 0)",message="is required unless actor.none is true",fieldPath=".from"
// +kubebuilder:validation:XValidation:rule="!(has(self.none) && self.none) || !has(self.from) || size(self.from) == 0",message="is set, but actor.none turns the self-drop off",fieldPath=".from"
// +kubebuilder:validation:XValidation:rule=`!(has(self.none) && self.none) || !has(self.whenMissing) || self.whenMissing == ""`,message="is set, but actor.none turns the self-drop off",fieldPath=".whenMissing"
type WebhookActor struct {
	// From lists the pointers that can name who caused an event, each compared with the identity. Required unless none is true,
	// which refuses it. At most 8, each at most 256 bytes.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.matches('^/(?:[^~]|~[01])*$'))",message="must be JSON Pointers that are not the root"
	// +optional
	From []string `json:"from,omitempty"`
	// None turns the self-drop off, for a source that has no actors.
	// +optional
	None bool `json:"none,omitempty"`
	// WhenMissing says what happens to an event in which no actor pointer resolves: drop (the default) or keep.
	// +kubebuilder:validation:Enum=drop;keep;""
	// +doc:default=drop
	// +optional
	WhenMissing WebhookWhenMissing `json:"whenMissing,omitempty"`
}

// WebhookThread names the field of an event's thread.
type WebhookThread struct {
	// Field is the name of an id field.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Field string `json:"field"`
	// Prefix comes before the field's value in the thread, such as pr-: letters, digits, '.', '_' and '-', at most 32 bytes.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]*$`
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// WebhookField is one named value of an event: from a first-of list of pointers into the body, or from a header. Set one of from and
// header.
//
// +kubebuilder:validation:XValidation:rule=`(has(self.from) && size(self.from) > 0) != (has(self.header) && self.header != "")`,message="set from or header, and not both"
type WebhookField struct {
	// Name is how the triggers and the template name the field: a lower-case letter, then letters and digits, at most 63 bytes.
	// key and state are reserved.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][A-Za-z0-9]*$`
	// +kubebuilder:validation:XValidation:rule="!(self in ['key', 'state'])",message="is reserved: the prompt view gives it a meaning"
	Name string `json:"name"`
	// From is a first-of list of pointers into the body. At most 8, each at most 256 bytes.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.matches('^/(?:[^~]|~[01])*$'))",message="must be JSON Pointers that are not the root"
	// +optional
	From []string `json:"from,omitempty"`
	// Header is a request header, of at most 64 bytes. It is not signed, so a header field is a filter hint only. A field may not
	// read verify.header, Authorization, Proxy-Authorization or Cookie.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^([-!#$%&'*+.^_|~0-9A-Za-z\x60]+)?$`
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['authorization', 'proxy-authorization', 'cookie'])",message="reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)"
	// +optional
	Header string `json:"header,omitempty"`
	// Kind is text (the default) or id. An id field's value is checked as an identifier, and the thread and the prompt template may
	// read it; a text field reaches the agent only as untrusted data.
	// +kubebuilder:validation:Enum=id;text;""
	// +doc:default=text
	// +optional
	Kind WebhookFieldKind `json:"kind,omitempty"`
}

// IsID says whether the field is of kind id.
func (f WebhookField) IsID() bool { return f.Kind == WebhookKindID }

// WebhookDelivery names where a delivery's ID is.
type WebhookDelivery struct {
	// Header is the request header that carries the source's ID of a delivery, such as X-GitHub-Delivery: an HTTP header name of
	// at most 64 bytes, not verify.header, Authorization, Proxy-Authorization or Cookie.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[-!#$%&'*+.^_|~0-9A-Za-z\x60]+$`
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['authorization', 'proxy-authorization', 'cookie'])",message="reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)"
	Header string `json:"header"`
}

// ValidWebhookPointer says whether s is a JSON Pointer that a webhook connection takes: not the root, a leading '/', '~' only as ~0
// or ~1, and at most MaxWebhookPointerLen bytes.
func ValidWebhookPointer(s string) bool {
	return len(s) <= MaxWebhookPointerLen && webhookPointer.MatchString(s)
}

// ValidWebhookChannel says whether s is a webhook channel, as a webhook Binding names it and as the platform must resolve it from an
// event: letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts, at most 140 bytes. It is the
// channel shape of the webhook platform's ChatPlatformOf entry, for a caller that holds no connection spec.
func ValidWebhookChannel(s string) bool {
	return len(s) <= maxWebhookChannelLen && webhookChannel.MatchString(s)
}

// webhookEventsOnly says whether every trigger of a Binding is webhookEvent, and there is one: such a Binding's channel and output are
// a webhook connection's. Any other Binding's are Slack's, as before the webhook platform. Call it after Default, which makes an empty
// list the mention trigger.
func (c *ChatBinding) webhookEventsOnly() bool {
	return len(c.Triggers) > 0 && !slices.ContainsFunc(c.Triggers, func(t Trigger) bool { return t.Type != TriggerWebhookEvent })
}

// webhook checks the spec.webhook of a connection: the identity and the actor, the verifier, the channel, the fields, the thread and
// the delivery header.
func (p *problems) webhook(h *WebhookSpec) {
	const at = "spec.webhook"
	switch {
	case h.Actor.None:
		if h.Identity != "" {
			p.add(at+".identity", "is set, but actor.none turns the self-drop off, which alone reads it")
		}
		if len(h.Actor.From) > 0 {
			p.add(at+".actor.from", "is set, but actor.none turns the self-drop off")
		}
		if h.Actor.WhenMissing != "" {
			p.add(at+".actor.whenMissing", "is set, but actor.none turns the self-drop off")
		}
	default:
		switch {
		case h.Identity == "":
			p.add(at+".identity", "is required unless actor.none is true: the agent's own events are dropped by it")
		case len(h.Identity) > MaxWebhookIdentityLen:
			p.maxBytes(at+".identity", h.Identity, MaxWebhookIdentityLen) // not shown: as long as it may be
		case !webhookIdentity.MatchString(h.Identity):
			p.add(at+".identity", "invalid identity %q: use the account as the source names it in the actor pointers: letters, digits and _.:@{}[]/-", h.Identity)
		}
		if len(h.Actor.From) == 0 {
			p.add(at+".actor.from", "is required unless actor.none is true")
		}
		if h.Actor.WhenMissing != "" {
			p.oneOf(at+".actor.whenMissing", string(h.Actor.WhenMissing), string(WebhookMissingDrop), string(WebhookMissingKeep))
		}
	}
	p.pointers(at+".actor.from", h.Actor.From)

	v := h.Verify
	p.oneOf(at+".verify.scheme", string(v.Scheme), string(WebhookHMACSHA256), string(WebhookToken))
	p.headerName(at+".verify.header", v.Header)
	switch {
	case len(v.Prefix) > MaxWebhookPrefixLen:
		p.maxBytes(at+".verify.prefix", v.Prefix, MaxWebhookPrefixLen)
	case !webhookPrefix.MatchString(v.Prefix):
		p.add(at+".verify.prefix", "must be printable ASCII: a header value holds no other")
	}
	if ts := v.Timestamp; ts != nil {
		if v.Scheme != WebhookHMACSHA256 {
			p.add(at+".verify.timestamp", "is taken by the hmac-sha256 scheme alone: the time is read after the signature passes")
		}
		if !ValidWebhookPointer(ts.From) {
			p.add(at+".verify.timestamp.from", "must be a JSON Pointer that is not the root, at most %d bytes", MaxWebhookPointerLen)
		}
		p.oneOf(at+".verify.timestamp.unit", string(ts.Unit), string(WebhookUnitMillis), string(WebhookUnitSeconds))
		if d := ts.Window.Duration; d <= 0 || d > MaxWebhookTimestampWindow {
			p.add(at+".verify.timestamp.window", "%s is out of range, must be above 0s and at most %s", d, MaxWebhookTimestampWindow)
		}
	}

	switch c := h.Channel; {
	case (len(c.From) == 0) == (c.Value == ""):
		p.add(at+".channel", "set from or value, and not both")
	case c.Value != "":
		p.webhookChannel(at+".channel.value", c.Value)
	}
	p.pointers(at+".channel.from", h.Channel.From)

	p.webhookFields(at, h)

	if th := h.Thread; th != nil {
		switch i := slices.IndexFunc(h.Fields, func(f WebhookField) bool { return f.Name == th.Field }); {
		case th.Field == "":
			p.add(at+".thread.field", "required")
		case i < 0:
			p.add(at+".thread.field", "%q is not a field of the connection", th.Field)
		case !h.Fields[i].IsID():
			p.add(at+".thread.field", "%q is not an id field", th.Field)
		}
		switch {
		case len(th.Prefix) > MaxWebhookPrefixLen:
			p.maxBytes(at+".thread.prefix", th.Prefix, MaxWebhookPrefixLen)
		case !webhookThreadPrefix.MatchString(th.Prefix):
			p.add(at+".thread.prefix", "invalid prefix %q: letters, digits, '.', '_' and '-'", th.Prefix)
		}
	}

	if d := h.Delivery; d != nil {
		if p.headerName(at+".delivery.header", d.Header) && readsCredential(d.Header, v.Header) {
			p.add(at+".delivery.header", "reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)")
		}
	}
}

// webhookFields checks the fields of h: their number, and each one's name, source and kind.
func (p *problems) webhookFields(at string, h *WebhookSpec) {
	if len(h.Fields) > MaxWebhookFields {
		p.add(at+".fields", "has %d items, maximum is %d", len(h.Fields), MaxWebhookFields)
		return // as for allowedNamespaces: a list over the cap is one problem, and its entries are not checked
	}
	seen := make(map[string]int, len(h.Fields))
	for i, f := range h.Fields {
		fp := fmt.Sprintf("%s.fields[%d]", at, i)
		switch {
		case f.Name == "":
			p.add(fp+".name", "required")
		case len(f.Name) > maxWebhookFieldNameLen:
			p.maxBytes(fp+".name", f.Name, maxWebhookFieldNameLen)
		case !webhookFieldName.MatchString(f.Name):
			p.add(fp+".name", "invalid name %q: a lower-case letter, then letters and digits", f.Name)
		case slices.Contains(webhookReservedNames, f.Name):
			p.add(fp+".name", "%q is reserved: the prompt view gives it a meaning", f.Name)
		}
		if first, dup := seen[f.Name]; dup && f.Name != "" {
			p.add(fp+".name", "duplicate of index %d", first)
		} else {
			seen[f.Name] = i
		}
		switch {
		case (len(f.From) == 0) == (f.Header == ""):
			p.add(fp, "set from or header, and not both")
		case f.Header != "":
			if p.headerName(fp+".header", f.Header) && readsCredential(f.Header, h.Verify.Header) {
				p.add(fp+".header", "reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)")
			}
		}
		p.pointers(fp+".from", f.From)
		if f.Kind != "" {
			p.oneOf(fp+".kind", string(f.Kind), string(WebhookKindID), string(WebhookKindText))
		}
	}
}

// webhookChannel checks a webhook channel: a fixed one of a connection, or a Binding's.
func (p *problems) webhookChannel(path, c string) {
	switch {
	case len(c) > maxWebhookChannelLen:
		p.maxBytes(path, c, maxWebhookChannelLen) // not shown: as long as it may be
	case !webhookChannel.MatchString(c):
		p.add(path, "invalid channel %q: letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts", c)
	}
}

// pointers checks a list of JSON Pointers.
func (p *problems) pointers(path string, from []string) {
	if len(from) > MaxWebhookFrom {
		p.add(path, "has %d items, maximum is %d", len(from), MaxWebhookFrom)
		return
	}
	for i, s := range from {
		if !ValidWebhookPointer(s) {
			p.add(fmt.Sprintf("%s[%d]", path, i), "must be a JSON Pointer that is not the root, at most %d bytes", MaxWebhookPointerLen)
		}
	}
}

// headerName checks a required HTTP header name and reports whether it is one.
func (p *problems) headerName(path, name string) bool {
	switch {
	case name == "":
		p.add(path, "required")
	case len(name) > maxWebhookHeaderNameLen:
		p.maxBytes(path, name, maxWebhookHeaderNameLen)
	case !httpHeaderName.MatchString(name):
		p.add(path, "invalid header name %q: letters, digits and !#$%%&'*+-.^_`|~", name)
	default:
		return true
	}
	return false
}

// readsCredential says whether the header name is one that carries a credential: the verifier's, or one of credentialHeaders.
func readsCredential(name, verifyHeader string) bool {
	return strings.EqualFold(name, verifyHeader) || slices.Contains(credentialHeaders, strings.ToLower(name))
}

// webhookLimitsHint says, to a webhookEvent trigger that lacks limits, that it needs both.
const webhookLimitsHint = "a webhookEvent trigger requires both rateLimit and maxRunsPerDay"

// webhookEventTrigger checks a webhookEvent trigger on its own: no from, extract or thread (the connection derives the actor and the
// thread); match and exclude named with field names, each with a regex; required limits; and a required template that parses, passes
// the walk and runs. That the names of match, exclude and the template are the connection's fields, and those the template reads its
// id fields, is checked when the broker reads the Binding with its connection (PlatformMismatches).
func (p *problems) webhookEventTrigger(path string, t *Trigger, b *regexBudget) {
	for _, f := range []struct {
		name string
		set  bool
	}{{"from", t.From != nil}, {"extract", len(t.Extract) > 0}, {"thread", t.Thread.isSet()}} {
		if f.set {
			p.add(path+"."+f.name, "not allowed with type: webhookEvent")
		}
	}
	for _, m := range []struct {
		name    string
		entries map[MessageField]string
	}{{"match", t.Match}, {"exclude", t.Exclude}} {
		at := path + "." + m.name
		// The schema's maxProperties is the field's, whatever the type: six, one for each message field of the other triggers.
		if len(m.entries) > len(messageFields()) {
			p.add(at, "has %d entries, maximum is %d", len(m.entries), len(messageFields()))
			continue
		}
		for i, f := range slices.Sorted(maps.Keys(m.entries)) {
			if p.longFieldName(at, i, string(f)) {
				continue
			}
			if !webhookFieldName.MatchString(string(f)) {
				p.add(at+"."+string(f), "invalid field name %q: a lower-case letter, then letters and digits", f)
			}
			p.regex(at+"."+string(f), m.entries[f], b)
		}
	}
	p.limits(path+".limits", t.Limits, webhookLimitsHint)
	switch {
	case t.PromptTemplate == "":
		p.add(path+".promptTemplate", "required")
	case len(t.PromptTemplate) > maxPromptTemplateLen:
		p.maxBytes(path+".promptTemplate", t.PromptTemplate, maxPromptTemplateLen) // refused unparsed
	default:
		if err := checkWebhookPromptTemplate("promptTemplate", t.PromptTemplate); err != nil {
			p.add(path+".promptTemplate", "invalid template: %v", err)
		}
	}
}

// longFieldName is longName with the cap of a webhook field's name.
func (p *problems) longFieldName(path string, index int, name string) bool {
	if len(name) <= maxWebhookFieldNameLen {
		return false
	}
	p.add(path, "entry %d, in name order, is named with %d bytes, longer than the %d byte limit", index, len(name), maxWebhookFieldNameLen)
	return true
}

// checkWebhookPromptTemplate is checkPromptTemplate for a webhookEvent trigger, whose .Fields are the connection's, which a Binding
// does not hold: the walk takes every name it reads as known, and the run gives each of those names a placeholder.
func checkWebhookPromptTemplate(name, text string) error {
	tmpl, err := ParsePromptTemplate(name, text)
	if err != nil {
		return err
	}
	refs, err := TemplateRefs(tmpl)
	if err != nil {
		return err
	}
	// An event has a channel and the connection's fields, and none of a message's state, key, link or source.
	for _, v := range refs.View {
		if v != "Channel" {
			return fmt.Errorf("reads .%s, which a webhookEvent trigger does not have: it reads .Channel and .Fields.<name> alone", v)
		}
	}
	extract := make(map[string]Extract, len(refs.Fields))
	for _, f := range refs.Fields {
		extract[f] = Extract{}
	}
	return checkPromptTemplate(name, text, extract)
}

// webhookTriggerMismatches are what a webhookEvent trigger at path names that the connection with fields does not have: a match or
// exclude key that is not a field, and a field that the template reads that is not an id field.
func webhookTriggerMismatches(path string, t Trigger, fields []WebhookField) []PlatformMismatch {
	var out []PlatformMismatch
	unsupported := func(path, problem, fix string) {
		out = append(out, PlatformMismatch{Path: path, Unsupported: true, Problem: problem, Fix: fix})
	}
	kinds := make(map[string]WebhookFieldKind, len(fields))
	for _, f := range fields {
		kinds[f.Name] = f.Kind
	}
	for _, m := range []struct {
		name    string
		entries map[MessageField]string
	}{{"match", t.Match}, {"exclude", t.Exclude}} {
		for _, f := range slices.Sorted(maps.Keys(m.entries)) {
			if _, ok := kinds[string(f)]; !ok {
				unsupported(path+"."+m.name+"."+string(f), "is not a field of the connection", "name a field of the connection's spec.webhook.fields")
			}
		}
	}
	tmpl, err := ParsePromptTemplate("promptTemplate", t.PromptTemplate)
	if err != nil {
		return out // Validate refused it
	}
	refs, err := TemplateRefs(tmpl)
	if err != nil {
		return out
	}
	const fix = "read only id fields: a text field reaches the agent as untrusted data alone"
	for _, name := range refs.Fields {
		switch kind, ok := kinds[name]; {
		case !ok:
			unsupported(path+".promptTemplate", fmt.Sprintf("reads .Fields.%s, which is not a field of the connection", name), fix)
		case kind != WebhookKindID:
			unsupported(path+".promptTemplate", fmt.Sprintf("reads .Fields.%s, which is not an id field", name), fix)
		}
	}
	return out
}
