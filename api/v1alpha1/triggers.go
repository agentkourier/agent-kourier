package v1alpha1

import (
	"cmp"
	"io"
	"maps"
	"slices"
	"strings"
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
	// Type is mention, a person @mentioning Agent Kourier, or botMessage, a message a bot posts.
	// A mention trigger takes no other field. The key is type, not on: YAML 1.1 reads a bare
	// on as the boolean true, and the loader's decoder follows it.
	// +kubebuilder:validation:Enum=mention;botMessage
	Type TriggerType `json:"type"`
	// From names the bot a botMessage trigger accepts. Required for botMessage.
	From *BotSource `json:"from,omitempty"`
	// Preset fills in match, extract, thread, limits and promptTemplate for a known sender. What the
	// Binding sets itself overrides the preset field by field: a match, exclude or extract entry replaces the
	// preset's entry of that name, and each part of thread and limits replaces the preset's (see applyPreset).
	// +kubebuilder:validation:Enum=alertmanager
	// +optional
	Preset string `json:"preset,omitempty"`
	// Match maps a message field to a regex it must match. Every entry must match.
	// +doc:default=From preset, if set
	Match map[MessageField]string `json:"match,omitempty"`
	// Exclude maps a message field to a regex; a message matching any entry is ignored.
	// +doc:default=From preset, if set
	Exclude map[MessageField]string `json:"exclude,omitempty"`
	// Extract names the values to pull out of a message. A message in which one of these
	// regexes does not match does not match the trigger.
	// +doc:default=From preset, if set
	Extract map[string]Extract `json:"extract,omitempty"`
	// Thread ties messages that share an extracted key to one thread.
	// +optional
	// +doc:default=From preset, if set
	Thread TriggerThread `json:"thread,omitzero"`
	// PromptTemplate is a Go template over the trigger's view of the message (.Channel,
	// .State, .Key, .Fields, .Link, .Source). Required for botMessage, directly or from the
	// preset.
	// +doc:default=From preset, if set
	PromptTemplate string `json:"promptTemplate,omitempty"`
	// Limits bounds how many investigations the trigger starts (rate, concurrency and a daily cap). What the
	// Binding leaves out comes from the preset; a trigger with no preset and no limits is unlimited.
	// +optional
	// +doc:default=From preset, if set
	Limits TriggerLimits `json:"limits,omitzero"`
}

// BotSource identifies the bot whose messages a trigger accepts.
type BotSource struct {
	// BotID is the Slack bot ID, never a display name. Agent Kourier's own bot ID is refused at
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
	Regex string `json:"regex"`
}

// TriggerThread decides which thread a message joins and when it closes the thread.
type TriggerThread struct {
	// By is key: messages with the same extracted "key" share a thread.
	// +kubebuilder:validation:Enum=key
	// +optional
	// +doc:default=From preset, if set
	By ThreadBy `json:"by,omitempty"`
	// Cooldown is how long after a turn the same key starts no new turn. Required with by,
	// and between 1m and 168h.
	// +optional
	// +doc:default=From preset, if set
	Cooldown metav1.Duration `json:"cooldown,omitzero"`
	// MaxIdle is how long an open thread may go without a message of its key before Agent Kourier counts it
	// abandoned, so that the key's next fire starts a new investigation instead of a note. It covers a
	// resolve that never arrived. It needs by, is at least twice the cooldown, and is at most 720h; unset
	// means the larger of 24h and twice the cooldown (see EffectiveMaxIdle). It must be longer than the
	// sender's repeat interval (Alertmanager's repeat_interval): an alert that keeps firing repeats at that
	// interval, and a maxIdle shorter than it counts a live alert abandoned between two repeats and
	// starts a second investigation for it. Twice the cooldown covers that only for the preset's 4h
	// cooldown against Alertmanager's 4h repeat_interval; a longer repeat_interval needs a maxIdle set
	// above it.
	// +optional
	// +doc:default=Larger of `24h` and twice the cooldown
	MaxIdle metav1.Duration `json:"maxIdle,omitzero"`
	// CloseWhen maps an extracted field to the value that closes the thread: Agent Kourier posts a
	// note and starts no turn. Every entry must equal.
	// +optional
	// +doc:default=From preset, if set
	CloseWhen map[string]string `json:"closeWhen,omitempty"`
}

// MaxThreadMaxIdle is the most idle time a trigger can mean (720h): the ceiling validation holds a set
// thread.maxIdle to, above the default of the longest cooldown. A sweep of the rows of idle threads must cut off
// no later than now minus this, or it would delete a thread that a trigger with a long maxIdle still counts live.
const MaxThreadMaxIdle = 30 * 24 * time.Hour

// DefaultMaxIdle is the idle time after which an open thread is abandoned, when the trigger does not say.
const DefaultMaxIdle = 24 * time.Hour

// EffectiveMaxIdle is MaxIdle, or, if it is unset, the larger of DefaultMaxIdle and twice the cooldown, so that
// the default always satisfies the rule that validation puts on a MaxIdle that is set.
func (t TriggerThread) EffectiveMaxIdle() time.Duration {
	if t.MaxIdle.Duration != 0 {
		return t.MaxIdle.Duration
	}
	return max(DefaultMaxIdle, 2*t.Cooldown.Duration)
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
// The thread merges by its parts (by, cooldown, closeWhen as a whole), the limits by theirs
// (each of rateLimit's fields, and the daily cap), and the prompt template is taken whole. Applying it twice changes nothing. An unknown preset is left for
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
	t.Limits = t.Limits.withDefaults(p.Limits)
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
// "RESOLVED - <alertname>", optionally behind a bracketed tag such as "[kind] " (a receiver
// can prefix its title to tell clusters apart), and whose fallback is Alertmanager's default,
// "[FIRING:n] <group label values> (<other common labels>) | <link>". Both are required, and
// the title is what keeps out Grafana's "FIRING: <rule> (<namespace>)" and the heartbeat. The
// thread key is the group label values from the fallback, without the count, so the same
// alert group shares a thread across repeats. The preset names no receiver: a Binding that
// shares its channel with other receivers narrows it with match.titleLink.
func presetTrigger(name string) (Trigger, bool) {
	if name != PresetAlertmanager {
		return Trigger{}, false
	}
	const (
		tag   = `(?:\[[^\]]{1,32}\] )?` // an optional title prefix, at most 32 bytes
		alert = `(?:FIRING \(\d+\)|RESOLVED)`
	)
	return Trigger{
		Match: map[MessageField]string{
			FieldTitle:    `^` + tag + alert + ` - `,
			FieldFallback: `^\[(?:FIRING(?::\d+)?|RESOLVED)\] `,
		},
		Extract: map[string]Extract{
			extractState: {Field: FieldTitle, Regex: `^` + tag + `(?P<state>FIRING|RESOLVED)`},
			extractKey:   {Field: FieldFallback, Regex: `^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])`},
			"alertname":  {Field: FieldTitle, Regex: `^` + tag + alert + ` - (?P<alertname>.+)$`},
		},
		Thread: TriggerThread{
			By:        ThreadByKey,
			Cooldown:  metav1.Duration{Duration: 4 * time.Hour},
			CloseWhen: map[string]string{extractState: "RESOLVED"},
		},
		// Five investigations in 10 minutes, two at a time, 40 a day. The cooldown already folds repeats of an alert into
		// its thread, so these bound distinct alerts, such as a storm.
		Limits: TriggerLimits{
			RateLimit:     &RateLimit{MaxRuns: 5, Per: metav1.Duration{Duration: 10 * time.Minute}, MaxConcurrent: 2},
			MaxRunsPerDay: new(int32(40)),
		},
		PromptTemplate: `An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
Investigate it with your read-only tools. Report the likely cause, the evidence you
checked, and the next steps a person should take.
`,
	}, true
}

// PromptView is the data a trigger's prompt template runs over. Load-time validation walks each
// template (TemplateRefs) and runs it once against a PromptView, so a mistyped field fails at
// load; the prompt renderer (internal/render, Prompt) renders this same type, and a field added
// here must get a rule there (the renderer refuses a field that has none).
//
// Fields, State and Key come out of an attacker's message (spec R2). Channel, Source and Link
// are Agent Kourier's own: the channel from the Binding, the source from the preset, and the link the
// Slack permalink that Agent Kourier builds from the channel and the ts of the triggering message. Link
// is never a field of the message such as titleLink, which the sender wrote. The renderer checks
// all of them anyway, as defence in depth. It writes a value into the instruction part only if
// the template reads it and it passes its rule; a value that fails refuses the render with a
// *render.RefusedError and nothing reaches the agent. A value the template does not read is not
// checked. The rules, which internal/render enforces and its tests pin:
//
//   - Fields values, State, Channel and Source: the alert-name shape, which is ASCII letters
//     and digits and "_", ".", "-" and ":", at most 128 bytes. The empty value passes.
//   - Key: the same, and "/", at most 128 bytes. A key of group label values holds spaces
//     (the alertmanager preset's does), so a template should read entries of .Fields, not .Key.
//   - Link: at most 256 bytes of ASCII letters, digits and "-._~:/?#=&" (so no "+" or "%",
//     which could carry encoded prose, and no white space, quote, angle bracket, "@" or
//     non-ASCII), an http or https scheme and a host. The empty value passes.
//
// A template may name values only as .Name, $.Name, .Fields.name, $.Fields.name or
// index .Fields "name"; see TemplateRefs.
type PromptView struct {
	// Channel is the channel the message arrived in.
	Channel string
	// State is the extracted "state" value, empty if the trigger extracts none.
	State string
	// Key is the extracted "key" value, empty if the trigger extracts none.
	Key string
	// Link is the Slack permalink of the triggering message, built by Agent Kourier from the channel
	// and the message's ts. It is not taken from the message: never a field such as titleLink.
	Link string
	// Source names what sent the message, such as the preset.
	Source string
	// Fields holds one value per extract entry, by the entry's name.
	Fields map[string]string
}

// ParsePromptTemplate parses a trigger's prompt template. Load-time validation and the prompt
// renderer both use it, so a template that loads can be rendered with the same functions.
// Missing map keys are errors when it runs, so a typo in .Fields.name fails loudly.
func ParsePromptTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(text)
}

// placeholderLen is the length of the placeholder values checkPromptTemplate runs a template
// against. It is long, so a template that slices a value ({{ slice .Key 0 10 }}) loads. The
// trade-off is accepted: a slice longer than a real value ({{ slice .State 0 10 }} on
// "FIRING") passes the load and fails when the first alert renders. The renderer returns that as
// an error, and CT-4b surfaces render errors like refusals (audited, no turn).
const placeholderLen = 64

// checkPromptTemplate parses text, walks its parse trees so that every reference is checked in
// every branch (see TemplateRefs; each .Fields entry must also be an extract of the trigger),
// and then runs it once against a view whose values are placeholders, for the errors a walk
// cannot see, such as a function called with the wrong arguments. The parse is the renderer's
// (ParsePromptTemplate), so a template that passes here parses there.
func checkPromptTemplate(name, text string, extract map[string]Extract) error {
	tmpl, err := ParsePromptTemplate(name, text)
	if err != nil {
		return err
	}
	w := newRefWalker(slices.Collect(maps.Keys(extract)), true)
	if err := w.walkAll(tmpl); err != nil {
		return err
	}
	placeholder := strings.Repeat("x", placeholderLen)
	view := PromptView{
		Channel: placeholder, State: placeholder, Key: placeholder, Link: placeholder, Source: placeholder,
		Fields: make(map[string]string, len(extract)),
	}
	for n := range extract {
		view.Fields[n] = placeholder
	}
	return tmpl.Execute(io.Discard, view)
}
