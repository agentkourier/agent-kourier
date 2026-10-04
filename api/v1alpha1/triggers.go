package v1alpha1

import (
	"cmp"
	"text/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Enumerations for chat triggers.
type (
	// TriggerType selects what kind of chat event a Trigger reacts to.
	TriggerType string
	// MessageField names one field of a chat message that a trigger's regexes read. The Slack
	// adapter flattens a legacy attachment into these neutral fields.
	MessageField string
	// ThreadBy selects what ties messages to one thread.
	ThreadBy string
)

// Values of the trigger enumerations, and the only preset in v1.
const (
	TriggerMention    TriggerType = "mention"
	TriggerBotMessage TriggerType = "botMessage"

	FieldText           MessageField = "text"
	FieldTitle          MessageField = "title"
	FieldTitleLink      MessageField = "titleLink"
	FieldFallback       MessageField = "fallback"
	FieldFooter         MessageField = "footer"
	FieldAttachmentText MessageField = "attachmentText"

	// ThreadByKey groups messages by the extracted field named "key".
	ThreadByKey ThreadBy = "key"

	// PresetAlertmanager is the preset for Alertmanager's Slack receiver (see Trigger.Preset).
	PresetAlertmanager = "alertmanager"
)

// Extract names that carry meaning for the thread and the prompt: ThreadByKey groups by
// "key", and a closeWhen rule usually tests "state".
const (
	extractKey   = "key"
	extractState = "state"
)

// messageFields lists the MessageField values, in the order error messages show them.
func messageFields() []MessageField {
	return []MessageField{FieldText, FieldTitle, FieldTitleLink, FieldFallback, FieldFooter, FieldAttachmentText}
}

// Trigger is one way an event in the bound channel starts or continues a turn (spec 8.2).
// Triggers are tried in order and the first that matches handles the event.
type Trigger struct {
	// Type is mention, a person @mentioning courier, or botMessage, a message a bot posts.
	// A mention trigger takes no other field. The key is type, not on: YAML 1.1 reads a bare
	// on as the boolean true, and the loader's decoder follows it.
	// +kubebuilder:validation:Enum=mention;botMessage
	Type TriggerType `json:"type"`
	// From names the bot a botMessage trigger accepts. Required for botMessage.
	From *BotSource `json:"from,omitempty"`
	// Preset fills in match, extract, thread and promptTemplate for a known sender. What the
	// Binding sets itself overrides the preset field by field (see applyPreset).
	// +kubebuilder:validation:Enum=alertmanager
	// +optional
	Preset string `json:"preset,omitempty"`
	// Match maps a message field to a regex it must match. Every entry must match.
	Match map[MessageField]string `json:"match,omitempty"`
	// Exclude maps a message field to a regex; a message matching any entry is ignored.
	Exclude map[MessageField]string `json:"exclude,omitempty"`
	// Extract names the values to pull out of a message. A message in which one of these
	// regexes does not match does not match the trigger.
	Extract map[string]Extract `json:"extract,omitempty"`
	// Thread ties messages that share an extracted key to one thread.
	// +optional
	Thread TriggerThread `json:"thread,omitzero"`
	// PromptTemplate is a Go template over the trigger's view of the message (.Channel,
	// .State, .Key, .Fields, .Link, .Source). Required for botMessage, directly or from the
	// preset.
	PromptTemplate string `json:"promptTemplate,omitempty"`
	// Per-trigger rate limits belong here (CT-5). There is no field for them yet.
}

// BotSource identifies the bot whose messages a trigger accepts.
type BotSource struct {
	// BotID is the Slack bot ID, never a display name. Courier's own bot ID is refused at
	// runtime: the load cannot know it.
	// +kubebuilder:validation:Pattern=`^B[A-Z0-9]+$`
	BotID string `json:"botId"`
}

// Extract pulls one named value out of a message field. The entry's name in Trigger.Extract
// must also be the name of a named group in Regex, which supplies the value.
type Extract struct {
	// Field is the message field the regex runs on.
	// +kubebuilder:validation:Enum=text;title;titleLink;fallback;footer;attachmentText
	Field MessageField `json:"field"`
	// Regex is a Go (RE2) regular expression of at most 512 bytes.
	// +kubebuilder:validation:MaxLength=512
	Regex string `json:"regex"`
}

// TriggerThread decides which thread a message joins and when it closes the thread.
type TriggerThread struct {
	// By is key: messages with the same extracted "key" share a thread.
	// +kubebuilder:validation:Enum=key
	// +optional
	By ThreadBy `json:"by,omitempty"`
	// Cooldown is how long after a turn the same key starts no new turn. Required with by,
	// and between 1m and 168h.
	// +optional
	Cooldown metav1.Duration `json:"cooldown,omitzero"`
	// CloseWhen maps an extracted field to the value that closes the thread: courier posts a
	// note and starts no turn. Every entry must equal.
	// +optional
	CloseWhen map[string]string `json:"closeWhen,omitempty"`
}

// defaultTriggers makes an empty trigger list the mention trigger, which is how a Binding
// behaved before triggers existed, and applies each trigger's preset.
func (c *ChatBinding) defaultTriggers() {
	if len(c.Triggers) == 0 {
		c.Triggers = []Trigger{{Type: TriggerMention}}
		return
	}
	for i := range c.Triggers {
		c.Triggers[i].applyPreset()
	}
}

// applyPreset fills what the trigger leaves unset from its preset, so a field the Binding
// sets always wins. The maps (match, exclude, extract) merge by key: an entry the Binding
// sets replaces the preset's entry of the same name and the preset's other entries stay.
// The thread merges by its parts (by, cooldown, closeWhen as a whole), and the prompt
// template is taken whole. Applying it twice changes nothing. An unknown preset is left for
// Validate to reject.
func (t *Trigger) applyPreset() {
	if t.Type != TriggerBotMessage {
		return
	}
	p, ok := presetTrigger(t.Preset)
	if !ok {
		return
	}
	t.Match = fillMissing(t.Match, p.Match)
	t.Exclude = fillMissing(t.Exclude, p.Exclude)
	t.Extract = fillMissing(t.Extract, p.Extract)
	t.Thread.By = cmp.Or(t.Thread.By, p.Thread.By)
	t.Thread.Cooldown.Duration = cmp.Or(t.Thread.Cooldown.Duration, p.Thread.Cooldown.Duration)
	if len(t.Thread.CloseWhen) == 0 {
		t.Thread.CloseWhen = p.Thread.CloseWhen
	}
	t.PromptTemplate = cmp.Or(t.PromptTemplate, p.PromptTemplate)
}

// fillMissing adds the entries of defaults that dst lacks and returns dst, which is a new map
// if dst was nil and there is something to add.
func fillMissing[K comparable, V any](dst, defaults map[K]V) map[K]V {
	if dst == nil && len(defaults) > 0 {
		dst = make(map[K]V, len(defaults))
	}
	for k, v := range defaults {
		if _, set := dst[k]; !set {
			dst[k] = v
		}
	}
	return dst
}

// presetTrigger returns a fresh copy of the named preset, as data in code: the fields a
// Binding can leave out for that sender. It reports false for an unknown name.
//
// alertmanager covers a Slack receiver whose title reads "FIRING (n) - <alertname>" or
// "RESOLVED - <alertname>" and whose fallback is Alertmanager's default,
// "[FIRING:n] <group label values> (<other common labels>) | <link>". Both are required, and
// the title is what keeps out Grafana's "FIRING: <rule> (<namespace>)" and the heartbeat. The
// thread key is the group label values from the fallback, without the count, so the same
// alert group shares a thread across repeats.
func presetTrigger(name string) (Trigger, bool) {
	if name != PresetAlertmanager {
		return Trigger{}, false
	}
	return Trigger{
		Match: map[MessageField]string{
			FieldTitle:    `^(?:FIRING \(\d+\)|RESOLVED) - `,
			FieldFallback: `^\[(?:FIRING(?::\d+)?|RESOLVED)\] `,
		},
		Extract: map[string]Extract{
			extractState: {Field: FieldTitle, Regex: `^(?P<state>FIRING|RESOLVED)`},
			extractKey:   {Field: FieldFallback, Regex: `^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])`},
			"alertname":  {Field: FieldTitle, Regex: `^(?:FIRING \(\d+\)|RESOLVED) - (?P<alertname>.+)$`},
		},
		Thread: TriggerThread{
			By:        ThreadByKey,
			Cooldown:  metav1.Duration{Duration: 4 * time.Hour},
			CloseWhen: map[string]string{extractState: "RESOLVED"},
		},
		PromptTemplate: `An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
Investigate it with your read-only tools. Report the likely cause, the evidence you
checked, and the next steps a person should take.
`,
	}, true
}

// ParsePromptTemplate parses a trigger's prompt template. Load-time validation and the prompt
// renderer both use it, so a template that loads can be rendered with the same functions.
// Missing map keys are errors at render time, so a typo in .Fields.name fails loudly.
func ParsePromptTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(text)
}
