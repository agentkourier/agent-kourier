package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Enumerations. Each type's values are also listed in its field's Enum marker.
//
// A field that Default fills from "" (output, progress, threadReplies, botMentions, from.kind), and thread.by, which
// Validate reads as unset when empty, list "" in their Enum marker too, and the emoji pattern accepts it: an object that
// says `output: ""` is one the broker accepts, so the schema must not refuse it (D50: nothing the schema refuses may be
// accepted by the broker). A field that Validate refuses when it is empty (dialect, platform, trigger type, extract
// field) does not.
type (
	// Dialect selects how Agent Kourier speaks to an agent backend.
	Dialect string
	// OutputMode selects how an agent's output is rendered into a thread.
	OutputMode string
	// ProgressMode selects how much of the agent's work a reply shows while it is being written (spec 8.3).
	ProgressMode string
	// ThreadReplies selects which replies in a thread Agent Kourier owns are turns (spec 8.2).
	ThreadReplies string
	// BotMentions selects whether another bot's mention of Agent Kourier starts a turn (spec 8.2).
	BotMentions string
)

// Values of the enumerations, plus the only platform and alert source type in v1.
const (
	DialectA2A      Dialect = "a2a"
	DialectKagentV1 Dialect = "kagent-v1"

	OutputLive  OutputMode = "live"
	OutputFinal OutputMode = "final"

	ProgressSteps            ProgressMode = "steps"
	ProgressStepsAndActivity ProgressMode = "steps-and-activity"
	ProgressActivity         ProgressMode = "activity"

	ThreadRepliesAll     ThreadReplies = "all"
	ThreadRepliesMention ThreadReplies = "mention"

	BotMentionsAny  BotMentions = "any"
	BotMentionsNone BotMentions = "none"

	PlatformSlack = "slack"
)

// ObjectRef names a resource in a namespace. An empty namespace means the namespace of the
// referring object. A Binding's agent.backendRef and chat.connectionRef may name an AgentBackend or a
// ChatConnection in another namespace only if that resource lists the Binding's namespace in its
// allowedNamespaces.
type ObjectRef struct {
	// Namespace is a DNS-1123 label: at most 63 lowercase letters, digits or '-', starting and ending with a letter or
	// digit.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	// +doc:default=Namespace of the referring object
	Namespace string `json:"namespace,omitempty"`
	// Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a
	// letter or digit.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// NamespacedName returns the "namespace/name" by which Agent Kourier names a resource in a conversation
// key, a message ID, an audit row or a log line. It does no defaulting: callers resolve a
// reference with an empty namespace to the referrer's namespace first.
func NamespacedName(namespace, name string) string { return namespace + "/" + name }

// DefaultSecretKey is the Secret key a SecretKeyRef reads when Key is empty (spec section 7).
const DefaultSecretKey = "token"

// SecretKeyRef names one value in a Secret in the referring object's namespace.
type SecretKeyRef struct {
	// Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a
	// letter or digit.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// Key selects the entry of the Secret. It defaults to "token". It consists of at most 253 alphanumeric characters,
	// '-', '_' or '.', and is neither "." nor "..", nor does it start with "..".
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(\.?[-_a-zA-Z0-9][-._a-zA-Z0-9]*)?$`
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// SecretRef names a Secret in the referring object's namespace whose keys are fixed.
type SecretRef struct {
	// Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a
	// letter or digit.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// defaultKey sets the default Key on a ref that is set and has none.
func (r *SecretKeyRef) defaultKey() {
	if r != nil && r.Key == "" {
		r.Key = DefaultSecretKey
	}
}

// AgentBackend describes an A2A server that hosts agents. Owned by the platform team.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=akbackend,categories=agentkourier
type AgentBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec AgentBackendSpec `json:"spec"`
}

// AgentBackendList is a list of AgentBackend.
//
// +kubebuilder:object:root=true
type AgentBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []AgentBackend `json:"items"`
}

// AgentBackendSpec is the desired state of an AgentBackend.
type AgentBackendSpec struct {
	// +kubebuilder:validation:Enum=a2a;kagent-v1
	Dialect Dialect `json:"dialect"`
	// The URL rules below copy only what the broker's parse (url.Parse, in validate.go) is certain to refuse, and are
	// regular expressions, not CEL's url(), which is ParseRequestURI and throws on values the broker accepts (a
	// fragment; getHostname() is empty for a host that is only a port). url.Parse cuts the fragment off before it
	// checks anything, so a rule that reads past a "#" refuses what the broker accepts: each rule stops at the first
	// "#" (and the authority ends at "/", "?" or "#"). A URL that this parse refuses and the rules let through is
	// refused when the broker reads it. (Not in the doc comment below, which the reference pages print, while no CRD
	// ships.)

	// URL is the http or https base URL for kagent-v1, or the exact JSON-RPC endpoint for a2a.
	// The a2a card must be served at URL + "/.well-known/agent-card.json", as AX does.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule=`self.matches('(?i)^https?://[^/?#]')`,message="must be an http or https URL with a host"
	// +kubebuilder:validation:XValidation:rule=`!self.matches('(?i)^https?://[^/?#]*@')`,message="must not contain credentials; identity comes from the Binding"
	// +kubebuilder:validation:XValidation:rule=`!self.matches(r'^[^#]*[\x00-\x1f\x7f]')`,message="invalid URL: must not contain control characters"
	// +kubebuilder:validation:XValidation:rule=`!self.matches(r'(?i)^https?://[^/?#]*[ \\^\x60{|}]')`,message="invalid URL: the host must not contain a space, backslash, caret, backtick, brace or bar"
	URL string `json:"url"`
	// Headers are sent on every request. Omitted headers add no static headers. At most 32.
	// +kubebuilder:validation:MaxItems=32
	// +doc:default=Empty list
	Headers []Header `json:"headers,omitempty"`
	// AllowedNamespaces lists the other namespaces whose Bindings may reference this backend. Each is a DNS-1123 label.
	// An empty list permits references only from the backend's own namespace.
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +doc:default=Empty list
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// Header is a static HTTP header. Set exactly one of value and valueSecretRef; valueSecretRef's key
// defaults to "token", as for the other single-value Secret references. A header that carries or selects
// identity is refused, because the Binding identity owns it: Authorization, Proxy-Authorization, Cookie,
// X-User-Id, X-Agent-Name, X-Share-Token and X-Kagent-Insecure-Runtime-Identity.
//
// +kubebuilder:validation:XValidation:rule=`(has(self.value) && self.value != "") != has(self.valueSecretRef)`,message="set exactly one of value and valueSecretRef"
type Header struct {
	// The generated CRD schema counts characters, and so does CEL's size(), but Agent Kourier counts bytes, so the byte
	// rule below is on bytes(self). The reserved names are IsReservedHeader's, which compares the canonical form of
	// the name (http.CanonicalHeaderKey changes only the case of ASCII letters), so the rule compares lowerAscii.
	// (Not in the doc comment below, which the reference pages print, while no CRD ships.)

	// Name is at most 256 bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="size(bytes(self)) <= 256",message="is longer than the 256 byte limit"
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['authorization', 'proxy-authorization', 'cookie', 'x-user-id', 'x-agent-name', 'x-share-token', 'x-kagent-insecure-runtime-identity'])",message="header is reserved for the Binding identity"
	Name           string        `json:"name"`
	Value          string        `json:"value,omitempty"`
	ValueSecretRef *SecretKeyRef `json:"valueSecretRef,omitempty"`
}

// ChatConnection is a workspace connection. Owned by the platform team.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=akconnection,categories=agentkourier
type ChatConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec ChatConnectionSpec `json:"spec"`
}

// ChatConnectionList is a list of ChatConnection.
//
// +kubebuilder:object:root=true
type ChatConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []ChatConnection `json:"items"`
}

// ChatConnectionSpec is the desired state of a ChatConnection.
type ChatConnectionSpec struct {
	// +kubebuilder:validation:Enum=slack
	Platform string `json:"platform"`
	// CredentialsSecretRef names one Secret with the fixed keys botToken and appToken (Socket Mode).
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
	// AllowedNamespaces lists the other namespaces whose Bindings may reference this connection. Each is a DNS-1123 label.
	// An empty list permits references only from the connection's own namespace.
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +doc:default=Empty list
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// Binding connects one agent to one chat channel.
// Owned by an application team.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=akbinding,categories=agentkourier
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=`.spec.chat.channel`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Binding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec BindingSpec `json:"spec"`
	// Status is the observed state of the Binding. It is set through the status subresource and never in a file: the
	// file loader refuses a Binding file that sets it.
	// +optional
	Status BindingStatus `json:"status,omitzero"`
}

// BindingList is a list of Binding.
//
// +kubebuilder:object:root=true
type BindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []Binding `json:"items"`
}

// BindingSpec is the desired state of a Binding.
type BindingSpec struct {
	Agent    AgentRef    `json:"agent"`
	Identity Identity    `json:"identity"`
	Chat     ChatBinding `json:"chat"`
	// +optional
	Interactions Interactions `json:"interactions,omitzero"`
	// +optional
	Feedback Feedback `json:"feedback,omitzero"`
	// +optional
	Session Session `json:"session,omitzero"`
}

// BindingStatus is the observed state of a Binding.
type BindingStatus struct {
	// Conditions are the Binding's states, one per type. The observedGeneration of a condition is the generation of
	// the Binding it describes.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentRef names the agent on a backend.
type AgentRef struct {
	BackendRef ObjectRef `json:"backendRef"`
	// Name is the agent's name on the backend.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace is the agent's namespace on the backend: a DNS-1123 label of at most 63 lowercase letters, digits or
	// '-', starting and ending with a letter or digit.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
}

// Identity is the service identity every turn in the Binding runs as, whoever wrote the message. Set
// userId, tokenSecretRef, or both.
//
// +kubebuilder:validation:XValidation:rule=`(has(self.userId) && self.userId != "") || has(self.tokenSecretRef)`,message="set userId, tokenSecretRef, or both"
type Identity struct {
	// UserID is sent as X-User-Id when the backend runs in insecure mode.
	UserID string `json:"userId,omitempty"`
	// TokenSecretRef holds the OIDC bearer token.
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`
}

// ChatBinding places the Binding in a channel.
type ChatBinding struct {
	ConnectionRef ObjectRef `json:"connectionRef"`
	// Channel is the platform's channel ID, not its name (Slack: `C…` or `G…`).
	// +kubebuilder:validation:Pattern=`^[CG][A-Z0-9]+$`
	Channel string `json:"channel"`
	// Output defaults to live.
	// +kubebuilder:validation:Enum=live;final;""
	// +kubebuilder:default=live
	// +optional
	Output OutputMode `json:"output"`
	// Progress chooses what a live reply shows of the agent's work before its text arrives. steps shows a card for each
	// tool call; an alert thread's first post, which cannot carry cards, shows a status line for the latest call instead,
	// as every write does on a platform without cards. steps-and-activity shows those, and a Working… card (or line, in
	// that first post) whenever 3 seconds pass with nothing new, titled or stood in for by the agent's own progress line.
	// activity shows one Working… card (a line in an alert thread) that counts the steps, or the agent's working updates
	// until the first step is counted, and names no tool, no argument and nothing the agent said. A Binding that sets none
	// gets steps-and-activity. Output final ignores it.
	// +kubebuilder:validation:Enum=steps;steps-and-activity;activity;""
	// +kubebuilder:default=steps-and-activity
	// +optional
	Progress ProgressMode `json:"progress"`
	// ThreadReplies defaults to all: any person's reply in a thread Agent Kourier owns is a turn.
	// With mention, only a reply that mentions Agent Kourier is a turn, whether a mention or a
	// trigger started the thread; the next mentioned turn carries the other replies as
	// context. A typed answer to a pending question that takes text replies needs no mention.
	// +kubebuilder:validation:Enum=all;mention;""
	// +kubebuilder:default=all
	// +optional
	ThreadReplies ThreadReplies `json:"threadReplies"`
	// BotMentions defaults to any: on a Binding with a mention trigger, a mention of Agent Kourier by any bot or
	// incoming webhook that can post in the channel starts a turn in the mention's thread, with a fixed prompt and
	// held to fixed limits. The platform cannot tell an agent from any other bot. With none, a bot's mention starts
	// nothing. Agent Kourier's own posts, and a bot that a botMessage trigger names, are never taken as a mention.
	// +kubebuilder:validation:Enum=any;none;""
	// +kubebuilder:default=any
	// +optional
	BotMentions BotMentions `json:"botMentions"`
	// Acknowledgment controls the temporary reaction Agent Kourier adds to an accepted message while it is being processed.
	// It defaults to enabled with the platform's own reaction (Slack: hourglass_flowing_sand).
	// +optional
	Acknowledgment *ChatAcknowledgment `json:"acknowledgment,omitempty"`
	// Triggers decide which events in the channel start or continue a turn. Empty means
	// [{type: mention}]: a mention starts a session. A list that names triggers is the whole list. At most 50.
	// +kubebuilder:validation:MaxItems=50
	// +doc:default=`[{type: mention}]`
	// +optional
	Triggers []Trigger `json:"triggers,omitempty"`
	// Mentions are the people an agent may notify, by writing `@name` in its reply. Agent Kourier then posts a message in the
	// thread that mentions each one named, one message for each. A name not listed here notifies nobody, and an agent can never supply
	// a user ID. Empty means no mentions, and nothing is added to the prompt. At most 50.
	// +kubebuilder:validation:MaxItems=50
	// +listType=map
	// +listMapKey=name
	// +optional
	Mentions []ChatMention `json:"mentions,omitempty"`
	// MaxMentionsPerTurn is the most people one turn mentions, at most 10; the people after them are not mentioned. It defaults to 2.
	// 0 is refused: to notify nobody, leave mentions empty.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=2
	// +optional
	MaxMentionsPerTurn *int `json:"maxMentionsPerTurn,omitempty"`
}

// MentionCap is the most people one turn mentions: MaxMentionsPerTurn, or its default if it is not set.
func (c ChatBinding) MentionCap() int {
	if c.MaxMentionsPerTurn == nil {
		return DefaultMaxMentionsPerTurn
	}
	return *c.MaxMentionsPerTurn
}

// ChatMention is one person an agent may notify.
type ChatMention struct {
	// Name is what the agent writes, after an `@`: lowercase letters, digits and `-`, starting with a letter, at most 63
	// characters. `here`, `channel` and `everyone` are refused.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,62}$`
	// +kubebuilder:validation:XValidation:rule="!(self in ['here', 'channel', 'everyone'])",message="a broadcast name (here, channel or everyone) is reserved and cannot be declared"
	Name string `json:"name"`
	// User is the platform's user ID of the person (Slack: `U…` or `W…`). It comes only from here, never from the agent.
	// +kubebuilder:validation:Pattern=`^[UW][A-Z0-9]{2,20}$`
	User string `json:"user"`
}

// ChatAcknowledgment configures the temporary reaction on an accepted message.
type ChatAcknowledgment struct {
	// Enabled adds the reaction after the message is durably accepted and removes it when processing ends.
	// It defaults to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Emoji is the reaction name without colons. Left empty, the platform's own acknowledgment reaction is used
	// (Slack: hourglass_flowing_sand). The API fills in no default, so a stored Binding does not carry one platform's
	// reaction.
	// +kubebuilder:validation:Pattern=`^(?:[a-z0-9][a-z0-9+_-]*|[+-][a-z0-9][a-z0-9+_-]*)?$`
	// +optional
	Emoji string `json:"emoji,omitempty"`
}

// RateLimit allows MaxRuns agent runs per window and MaxConcurrent at once.
type RateLimit struct {
	// +kubebuilder:validation:Minimum=1
	MaxRuns int32 `json:"maxRuns"`
	// Per is the window length.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([-+]?0|[-+]?([0-9]*\.?[0-9]*(ns|us|µs|μs|ms|s|m|h))+)$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('24h')",message="is out of range, must be between 10s and 24h"
	Per metav1.Duration `json:"per"`
	// +kubebuilder:validation:Minimum=1
	MaxConcurrent int32 `json:"maxConcurrent"`
}

// Interactions configures agent pauses that wait on a person.
//
// +kubebuilder:validation:XValidation:rule="!has(self.approverGroups) || size(self.approverGroups) == 0 || (has(self.toolApprovals) && self.toolApprovals)",message="requires spec.interactions.toolApprovals: true",fieldPath=".approverGroups"
type Interactions struct {
	// AskUser lets the agent ask questions in the thread. Defaults to false.
	// +kubebuilder:default=false
	AskUser bool `json:"askUser,omitempty"`
	// ToolApprovals lets the agent ask a person to approve tool calls in the thread. Defaults to false.
	// +kubebuilder:default=false
	// +optional
	ToolApprovals bool `json:"toolApprovals,omitempty"`
	// ApproverGroups names the platform's user groups whose members may decide a tool approval, by the platform's ID (Slack: `S…`).
	// An empty list allows any non-bot channel member. At most 10 groups may be named. Group IDs start with S and contain
	// uppercase letters and digits; duplicates and empty entries are refused.
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:Pattern=`^S[A-Z0-9]+$`
	// +listType=set
	// +optional
	ApproverGroups []string `json:"approverGroups,omitempty"`
	// Timeout is how long a pause waits for an answer. Defaults to 30m.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="30m"
	// +kubebuilder:validation:Pattern=`^([-+]?0|[-+]?([0-9]*\.?[0-9]*(ns|us|µs|μs|ms|s|m|h))+)$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="must be positive; leave it out, or write 0, for the default"
	// +optional
	Timeout metav1.Duration `json:"timeout"`
}

// Feedback configures the useful and not-useful buttons. The final message of an investigation that
// a trigger started always carries them; Enabled adds them to the Binding's chat answers too.
type Feedback struct {
	// Enabled puts the buttons on the final message of an answer to a person's message in a thread. It does not
	// switch them off for an investigation a trigger started. Defaults to false.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// Session configures thread-to-session mapping.
type Session struct {
	// ThreadTTL is how long a thread keeps its session after its last activity. Defaults to 2160h
	// (90 days).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="2160h"
	// +kubebuilder:validation:Pattern=`^([-+]?0|[-+]?([0-9]*\.?[0-9]*(ns|us|µs|μs|ms|s|m|h))+)$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="must be positive; leave it out, or write 0, for the default"
	// +optional
	ThreadTTL metav1.Duration `json:"threadTTL"`
}

// Defaults for fields left empty by Default.
const (
	DefaultInteractionTimeout = 30 * time.Minute
	DefaultThreadTTL          = 90 * 24 * time.Hour
	DefaultMaxMentionsPerTurn = 2
)

// Default fills the fields left empty. A ChatConnection has none to fill; the method keeps the shape that
// every kind loaded from files has.
func (c *ChatConnection) Default() {}

// Default fills the fields left empty. Call it before Validate.
func (b *Binding) Default() {
	if b.Spec.Chat.Output == "" {
		b.Spec.Chat.Output = OutputLive
	}
	if b.Spec.Chat.Progress == "" {
		b.Spec.Chat.Progress = ProgressStepsAndActivity
	}
	if b.Spec.Chat.ThreadReplies == "" {
		b.Spec.Chat.ThreadReplies = ThreadRepliesAll
	}
	if b.Spec.Chat.BotMentions == "" {
		b.Spec.Chat.BotMentions = BotMentionsAny
	}
	if b.Spec.Chat.Acknowledgment == nil {
		b.Spec.Chat.Acknowledgment = &ChatAcknowledgment{}
	}
	if b.Spec.Chat.Acknowledgment.Enabled == nil {
		enabled := true
		b.Spec.Chat.Acknowledgment.Enabled = &enabled
	}
	if b.Spec.Chat.MaxMentionsPerTurn == nil {
		b.Spec.Chat.MaxMentionsPerTurn = new(DefaultMaxMentionsPerTurn)
	}
	b.Spec.Chat.defaultTriggers()
	if b.Spec.Interactions.Timeout.Duration == 0 {
		b.Spec.Interactions.Timeout.Duration = DefaultInteractionTimeout
	}
	if b.Spec.Session.ThreadTTL.Duration == 0 {
		b.Spec.Session.ThreadTTL.Duration = DefaultThreadTTL
	}
	b.Spec.Identity.TokenSecretRef.defaultKey()
}

// Default fills the fields left empty. Call it before Validate.
func (a *AgentBackend) Default() {
	for i := range a.Spec.Headers {
		a.Spec.Headers[i].ValueSecretRef.defaultKey()
	}
}
