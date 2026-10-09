package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Enumerations. Each type's values are also listed in its field's Enum marker.
type (
	// Dialect selects how Agent Kourier speaks to an agent backend.
	Dialect string
	// ChatMode selects how Agent Kourier receives chat events.
	ChatMode string
	// OutputMode selects how an agent's output is rendered into a thread.
	OutputMode string
	// ThreadReplies selects which replies in a thread Agent Kourier owns are turns (spec 8.2).
	ThreadReplies string
)

// Values of the enumerations, plus the only platform and alert source type in v1.
const (
	DialectA2A      Dialect = "a2a"
	DialectKagentV1 Dialect = "kagent-v1"

	ModeSocket ChatMode = "socket"
	ModeHTTP   ChatMode = "http"

	OutputLive  OutputMode = "live"
	OutputFinal OutputMode = "final"

	ThreadRepliesAll     ThreadReplies = "all"
	ThreadRepliesMention ThreadReplies = "mention"

	PlatformSlack     = "slack"
	AlertAlertmanager = "alertmanager"
)

// ObjectRef names a resource in a namespace. An empty Namespace means the namespace of the
// referring object. Whether a cross-namespace reference is allowed is decided by WP-21.
type ObjectRef struct {
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
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
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key selects the entry of the Secret. It defaults to "token".
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// SecretRef names a Secret in the referring object's namespace whose keys are fixed.
type SecretRef struct {
	// +kubebuilder:validation:MinLength=1
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
	// URL is the http or https base URL for kagent-v1, or the exact JSON-RPC endpoint for a2a.
	// The a2a card must be served at URL + "/.well-known/agent-card.json", as AX does.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`
	// Headers are sent on every request.
	Headers []Header `json:"headers,omitempty"`
	// AllowedNamespaces lists the namespaces whose Bindings may reference this backend.
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// Header is a static HTTP header. Set exactly one of Value and ValueSecretRef; a
// ValueSecretRef follows the same key convention as the other single-value refs.
// Authorization and X-User-Id are reserved for the Binding identity; Validate rejects them.
type Header struct {
	// +kubebuilder:validation:MinLength=1
	Name           string        `json:"name"`
	Value          string        `json:"value,omitempty"`
	ValueSecretRef *SecretKeyRef `json:"valueSecretRef,omitempty"`
}

// ChatConnection is a workspace connection. Owned by the platform team.
//
// +kubebuilder:object:root=true
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
	// Mode defaults to socket.
	// +kubebuilder:validation:Enum=socket;http
	// +kubebuilder:default=socket
	// +optional
	Mode ChatMode `json:"mode"`
	// CredentialsSecretRef names one Secret with the fixed keys botToken, appToken
	// (socket mode), and signingSecret (http mode).
	CredentialsSecretRef SecretRef       `json:"credentialsSecretRef"`
	DirectMessages       *DirectMessages `json:"directMessages,omitempty"`
	// AllowedNamespaces lists the namespaces whose Bindings may reference this connection.
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// DirectMessages configures direct messages to the bot.
type DirectMessages struct {
	// DefaultBindingRef is the Binding that answers direct messages.
	DefaultBindingRef *ObjectRef `json:"defaultBindingRef,omitempty"`
}

// Binding connects one agent to one chat channel and, optionally, one alert route.
// Owned by an application team.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Binding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   BindingSpec   `json:"spec"`
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
	// Alerts enables the alert webhook; without it the Binding is chat only.
	Alerts *Alerts `json:"alerts,omitempty"`
	// +optional
	Interactions Interactions `json:"interactions,omitzero"`
	// +optional
	Feedback Feedback `json:"feedback,omitzero"`
	// +optional
	Session Session `json:"session,omitzero"`
}

// AgentRef names the agent on a backend.
type AgentRef struct {
	BackendRef ObjectRef `json:"backendRef"`
	// Name is the agent's name on the backend.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace is the agent's namespace on the backend.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// Identity is the service identity every turn in the Binding runs as (D14). Set UserID,
// TokenSecretRef, or both.
type Identity struct {
	// UserID is sent as X-User-Id when the backend runs in insecure mode.
	UserID string `json:"userId,omitempty"`
	// TokenSecretRef holds the OIDC bearer token.
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`
}

// ChatBinding places the Binding in a channel.
type ChatBinding struct {
	ConnectionRef ObjectRef `json:"connectionRef"`
	// Channel is the channel ID, not its name.
	// +kubebuilder:validation:Pattern=`^[CG][A-Z0-9]+$`
	Channel string `json:"channel"`
	// Output defaults to live.
	// +kubebuilder:validation:Enum=live;final
	// +kubebuilder:default=live
	// +optional
	Output OutputMode `json:"output"`
	// ThreadReplies defaults to all: any person's reply in a thread Agent Kourier owns is a turn.
	// With mention, only a reply that mentions Agent Kourier is a turn, whether a mention or a
	// trigger started the thread; the next mentioned turn carries the other replies as
	// context. A typed answer to a pending question that takes text replies (spec 8.4)
	// needs no mention, and neither does a direct message.
	// +kubebuilder:validation:Enum=all;mention
	// +kubebuilder:default=all
	// +optional
	ThreadReplies ThreadReplies `json:"threadReplies"`
	// Triggers decide which events in the channel start or continue a turn (spec 8.2).
	// Empty means [{type: mention}], which is how a Binding behaved before triggers existed;
	// Default fills it in.
	// +optional
	Triggers []Trigger `json:"triggers,omitempty"`
}

// Alerts configures the Binding's alert webhook.
type Alerts struct {
	// +kubebuilder:validation:Enum=alertmanager
	Type string `json:"type"`
	// TokenSecretRef holds the webhook bearer token, in the Binding's namespace.
	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`
	// PromptTemplate is a Go template over the normalized AlertEvent.
	// +kubebuilder:validation:MinLength=1
	PromptTemplate string `json:"promptTemplate"`
	// RateLimit bounds agent runs per window; omitted means unlimited.
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
	// MaxRunsPerDay caps agent runs per day (D31); 0 means no cap.
	// +kubebuilder:validation:Minimum=0
	MaxRunsPerDay int32 `json:"maxRunsPerDay,omitempty"`
}

// RateLimit allows MaxRuns agent runs per window and MaxConcurrent at once.
type RateLimit struct {
	// +kubebuilder:validation:Minimum=1
	MaxRuns int32 `json:"maxRuns"`
	// Per is the window length.
	Per metav1.Duration `json:"per"`
	// +kubebuilder:validation:Minimum=1
	MaxConcurrent int32 `json:"maxConcurrent"`
}

// Interactions configures agent pauses that wait on a person.
type Interactions struct {
	// AskUser lets the agent ask questions in the thread. Defaults to false.
	AskUser bool `json:"askUser,omitempty"`
	// ToolApprovals gates tool calls on approval (Phase 5). Defaults to false.
	ToolApprovals bool `json:"toolApprovals,omitempty"`
	// Timeout is how long a pause waits for an answer. Defaults to 30m.
	// +kubebuilder:default="30m"
	// +optional
	Timeout metav1.Duration `json:"timeout"`
	// ApproverGroups is reserved (R1); empty means any channel member.
	ApproverGroups []string `json:"approverGroups,omitempty"`
}

// Feedback configures the useful and not-useful buttons (D27, spec 8.7). The final message of an investigation that
// a trigger started always carries them; Enabled adds them to the Binding's chat answers too.
type Feedback struct {
	// Enabled puts the buttons on the final message of an answer to a person's message in a thread. It does not
	// switch them off for an investigation a trigger started. Defaults to false.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// Session configures thread-to-session mapping.
type Session struct {
	// ThreadTTL is how long a thread keeps its session (D28). Defaults to 2160h (90 days).
	// +kubebuilder:default="2160h"
	// +optional
	ThreadTTL metav1.Duration `json:"threadTTL"`
}

// BindingStatus is the observed state of a Binding. Written by the controller.
type BindingStatus struct {
	// Conditions include Ready, BackendReachable, and ChannelJoined.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// WebhookPath is the path Alertmanager posts to; set only when Alerts is.
	WebhookPath string `json:"webhookPath,omitempty"`
}

// Defaults for fields left empty by Default.
const (
	DefaultInteractionTimeout = 30 * time.Minute
	DefaultThreadTTL          = 90 * 24 * time.Hour
)

// Default fills the fields left empty. Call it before Validate.
func (c *ChatConnection) Default() {
	if c.Spec.Mode == "" {
		c.Spec.Mode = ModeSocket
	}
}

// Default fills the fields left empty. Call it before Validate.
func (b *Binding) Default() {
	if b.Spec.Chat.Output == "" {
		b.Spec.Chat.Output = OutputLive
	}
	if b.Spec.Chat.ThreadReplies == "" {
		b.Spec.Chat.ThreadReplies = ThreadRepliesAll
	}
	b.Spec.Chat.defaultTriggers()
	if b.Spec.Interactions.Timeout.Duration == 0 {
		b.Spec.Interactions.Timeout.Duration = DefaultInteractionTimeout
	}
	if b.Spec.Session.ThreadTTL.Duration == 0 {
		b.Spec.Session.ThreadTTL.Duration = DefaultThreadTTL
	}
	b.Spec.Identity.TokenSecretRef.defaultKey()
	if b.Spec.Alerts != nil {
		b.Spec.Alerts.TokenSecretRef.defaultKey()
	}
}

// Default fills the fields left empty. Call it before Validate.
func (a *AgentBackend) Default() {
	for i := range a.Spec.Headers {
		a.Spec.Headers[i].ValueSecretRef.defaultKey()
	}
}
