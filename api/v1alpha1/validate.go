package v1alpha1

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// channelID matches Slack channel IDs; its literal must equal ChatBinding.Channel's Pattern marker.
var channelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

// botID matches Slack bot IDs; its literal must equal BotSource.BotID's Pattern marker.
var botID = regexp.MustCompile(`^B[A-Z0-9]+$`)

// extractName is what a template can reach as .Fields.<name>: a Go identifier that starts
// with a lower-case letter. It is also the name of the regex group that supplies the value.
var extractName = regexp.MustCompile(`^[a-z][A-Za-z0-9_]*$`)

// Limits on a trigger. A regex is capped because it is operator text compiled at load; the
// cooldown is bounded so a typo (4 for 4h, 400h for 4h) cannot silence a key for weeks or
// restart a turn on every message.
const (
	maxTriggerRegexLen = 512
	minThreadCooldown  = time.Minute
	maxThreadCooldown  = 7 * 24 * time.Hour
)

// IsReservedHeader reports whether name carries or could forge the Binding identity (D14),
// so an AgentBackend must not set it. Dialects refuse it too. The kagent alpha5 headers
// are reserved per D32 (WP-03, WP-20c review): X-Agent-Name selects the acting user via
// X-User-Id or ?user_id=; X-Share-Token grants the session owner's sessions on courier's
// route; X-Kagent-Insecure-Runtime-Identity is an unsigned actor identity kagent forwards
// as auth metadata.
func IsReservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "X-User-Id",
		"X-Agent-Name", "X-Share-Token", "X-Kagent-Insecure-Runtime-Identity":
		return true
	}
	return false
}

// problems collects validation failures, each prefixed with the path of the field.
type problems []error

func (p *problems) add(path, format string, args ...any) {
	*p = append(*p, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (p *problems) required(path, v string) {
	if v == "" {
		p.add(path, "required")
	}
}

func (p *problems) oneOf(path, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		p.add(path, "unsupported value %q, must be one of %s", v, strings.Join(allowed, ", "))
	}
}

func (p *problems) positive(path string, n int64) {
	if n <= 0 {
		p.add(path, "must be positive")
	}
}

func (p *problems) duration(path string, d metav1.Duration) { p.positive(path, int64(d.Duration)) }

// name checks a required DNS-1123 subdomain, the form the API server enforces on names.
func (p *problems) name(path, v string) {
	if v == "" {
		p.add(path, "required")
	} else if len(validation.IsDNS1123Subdomain(v)) > 0 {
		p.add(path, "invalid name %q: must be at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit", v)
	}
}

// namespace checks a required DNS-1123 label.
func (p *problems) namespace(path, v string) {
	if v == "" {
		p.add(path, "required")
	} else if len(validation.IsDNS1123Label(v)) > 0 {
		p.add(path, "invalid namespace %q: must be at most 63 lowercase letters, digits or '-', starting and ending with a letter or digit", v)
	}
}

func (p *problems) namespaces(path string, ns []string) {
	for i, n := range ns {
		p.namespace(fmt.Sprintf("%s[%d]", path, i), n)
	}
}

func (p *problems) meta(m metav1.ObjectMeta) {
	p.name("metadata.name", m.Name)
	p.namespace("metadata.namespace", m.Namespace)
}

func (p *problems) secretRef(path string, r *SecretKeyRef) {
	if r == nil {
		return
	}
	p.name(path+".name", r.Name)
	if r.Key != "" && len(validation.IsConfigMapKey(r.Key)) > 0 {
		p.add(path+".key", "invalid key %q: must consist of alphanumeric characters, '-', '_' or '.'", r.Key)
	}
}

func (p *problems) objectRef(path string, r *ObjectRef) {
	p.name(path+".name", r.Name)
	if r.Namespace != "" {
		p.namespace(path+".namespace", r.Namespace)
	}
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or check other objects (see WP-21).
func (a *AgentBackend) Validate() error {
	var p problems
	p.meta(a.ObjectMeta)
	s := &a.Spec
	p.oneOf("spec.dialect", string(s.Dialect), string(DialectA2A), string(DialectKagentV1))
	if s.URL == "" {
		p.add("spec.url", "required")
	} else if u, err := url.Parse(s.URL); err != nil {
		p.add("spec.url", "must be an http or https URL with a host")
	} else if u.User != nil {
		// net/http would turn userinfo into Basic auth, bypassing the Binding identity.
		p.add("spec.url", "must not contain credentials; identity comes from the Binding (D14)")
	} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.add("spec.url", "invalid URL %q: must be an http or https URL with a host", u.Redacted())
	}
	for i, h := range s.Headers {
		path := fmt.Sprintf("spec.headers[%d]", i)
		p.required(path+".name", h.Name)
		if IsReservedHeader(h.Name) {
			p.add(path+".name", "header %q is reserved for the Binding identity (D14)", h.Name)
		}
		if (h.Value == "") == (h.ValueSecretRef == nil) {
			p.add(path, "set exactly one of value and valueSecretRef")
		}
		p.secretRef(path+".valueSecretRef", h.ValueSecretRef)
	}
	p.namespaces("spec.allowedNamespaces", s.AllowedNamespaces)
	return errors.Join(p...)
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or check other objects (see WP-21).
func (c *ChatConnection) Validate() error {
	var p problems
	p.meta(c.ObjectMeta)
	s := &c.Spec
	p.oneOf("spec.platform", s.Platform, PlatformSlack)
	p.oneOf("spec.mode", string(s.Mode), string(ModeSocket), string(ModeHTTP))
	p.name("spec.credentialsSecretRef.name", s.CredentialsSecretRef.Name)
	if dm := s.DirectMessages; dm != nil && dm.DefaultBindingRef != nil {
		p.objectRef("spec.directMessages.defaultBindingRef", dm.DefaultBindingRef)
	}
	p.namespaces("spec.allowedNamespaces", s.AllowedNamespaces)
	return errors.Join(p...)
}

// Validate reports every per-object rule the object breaks, joined, each naming the field
// path. Call Default first; Validate treats unset defaulted fields as invalid. It does not
// resolve references or enforce one Binding per channel (see WP-21).
func (b *Binding) Validate() error {
	var p problems
	p.meta(b.ObjectMeta)
	s := &b.Spec
	p.objectRef("spec.agent.backendRef", &s.Agent.BackendRef)
	p.required("spec.agent.name", s.Agent.Name)
	p.namespace("spec.agent.namespace", s.Agent.Namespace)
	if s.Identity.UserID == "" && s.Identity.TokenSecretRef == nil {
		p.add("spec.identity", "set userId, tokenSecretRef, or both")
	}
	p.secretRef("spec.identity.tokenSecretRef", s.Identity.TokenSecretRef)
	p.objectRef("spec.chat.connectionRef", &s.Chat.ConnectionRef)
	if c := s.Chat.Channel; c == "" {
		p.add("spec.chat.channel", "required")
	} else if !channelID.MatchString(c) {
		p.add("spec.chat.channel", "invalid channel %q: must be a channel ID such as C0123456789, not a name", c)
	}
	p.oneOf("spec.chat.output", string(s.Chat.Output), string(OutputLive), string(OutputFinal))
	p.triggers("spec.chat.triggers", s.Chat.Triggers)
	if a := s.Alerts; a != nil {
		p.oneOf("spec.alerts.type", a.Type, AlertAlertmanager)
		p.secretRef("spec.alerts.tokenSecretRef", &a.TokenSecretRef)
		p.required("spec.alerts.promptTemplate", a.PromptTemplate)
		if r := a.RateLimit; r != nil {
			p.positive("spec.alerts.rateLimit.maxRuns", int64(r.MaxRuns))
			p.duration("spec.alerts.rateLimit.per", r.Per)
			p.positive("spec.alerts.rateLimit.maxConcurrent", int64(r.MaxConcurrent))
		}
		if a.MaxRunsPerDay < 0 {
			p.add("spec.alerts.maxRunsPerDay", "must not be negative")
		}
	}
	p.duration("spec.interactions.timeout", s.Interactions.Timeout)
	p.duration("spec.session.threadTTL", s.Session.ThreadTTL)
	return errors.Join(p...)
}

// regex checks that expr is a non-empty RE2 expression within the length cap, and returns it
// compiled, or nil if it is not.
func (p *problems) regex(path, expr string) *regexp.Regexp {
	if expr == "" {
		p.add(path, "required")
		return nil
	}
	if len(expr) > maxTriggerRegexLen {
		p.add(path, "regular expression is %d bytes, longer than the %d byte limit", len(expr), maxTriggerRegexLen)
		return nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		p.add(path, "invalid regular expression: %v", err)
		return nil
	}
	return re
}

// messageField checks that f names a field of a chat message.
func (p *problems) messageField(path string, f MessageField) {
	if !slices.Contains(messageFields(), f) {
		names := make([]string, 0, len(messageFields()))
		for _, n := range messageFields() {
			names = append(names, string(n))
		}
		p.add(path, "unsupported message field %q, must be one of %s", f, strings.Join(names, ", "))
	}
}

// fieldRegexes checks a match or exclude map: known fields, each with a valid regex.
func (p *problems) fieldRegexes(path string, m map[MessageField]string) {
	for _, f := range slices.Sorted(maps.Keys(m)) {
		at := path + "." + string(f)
		p.messageField(at, f)
		p.regex(at, m[f])
	}
}

// triggers validates the trigger list: each trigger on its own, then the list for triggers
// that could never be reached. Defaulting has already filled the presets in.
func (p *problems) triggers(path string, ts []Trigger) {
	seen := map[string]int{}
	for i := range ts {
		at := fmt.Sprintf("%s[%d]", path, i)
		t := &ts[i]
		p.trigger(at, t)
		// A trigger with the same bot, match and exclude as an earlier one matches the same
		// messages, so the later one would never run.
		var bot string
		if t.From != nil {
			bot = t.From.BotID
		}
		id := fmt.Sprintf("%q %q %q %q", t.Type, bot, t.Match, t.Exclude)
		if first, dup := seen[id]; dup {
			p.add(at, "duplicate of %s[%d]", path, first)
		} else {
			seen[id] = i
		}
	}
}

func (p *problems) trigger(path string, t *Trigger) {
	p.oneOf(path+".type", string(t.Type), string(TriggerMention), string(TriggerBotMessage))
	switch t.Type {
	case TriggerMention:
		p.mentionTrigger(path, t)
	case TriggerBotMessage:
		p.botMessageTrigger(path, t)
	}
}

// mentionTrigger rejects every field a mention trigger has no use for, so a field set by
// mistake does not silently do nothing.
func (p *problems) mentionTrigger(path string, t *Trigger) {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"from", t.From != nil}, {"preset", t.Preset != ""}, {"match", len(t.Match) > 0},
		{"exclude", len(t.Exclude) > 0}, {"extract", len(t.Extract) > 0},
		{"thread", t.Thread.By != "" || t.Thread.Cooldown.Duration != 0 || len(t.Thread.CloseWhen) > 0},
		{"promptTemplate", t.PromptTemplate != ""},
	} {
		if f.set {
			p.add(path+"."+f.name, "not allowed with type: mention")
		}
	}
}

func (p *problems) botMessageTrigger(path string, t *Trigger) {
	switch {
	case t.From == nil || t.From.BotID == "":
		p.add(path+".from.botId", "required")
	case !botID.MatchString(t.From.BotID):
		p.add(path+".from.botId", "invalid bot ID %q: must be a Slack bot ID such as B0123456789, not a name", t.From.BotID)
	}
	if t.Preset != "" {
		p.oneOf(path+".preset", t.Preset, PresetAlertmanager)
	}
	p.fieldRegexes(path+".match", t.Match)
	p.fieldRegexes(path+".exclude", t.Exclude)
	for _, name := range slices.Sorted(maps.Keys(t.Extract)) {
		p.extract(path+".extract."+name, name, t.Extract[name])
	}
	p.thread(path+".thread", &t.Thread, t.Extract)
	if t.PromptTemplate == "" {
		p.add(path+".promptTemplate", "required")
	} else if _, err := ParsePromptTemplate("promptTemplate", t.PromptTemplate); err != nil {
		p.add(path+".promptTemplate", "invalid template: %v", err)
	}
}

// extract checks one extract entry: a known field and a regex with a group named like the
// entry, which is where the value comes from.
func (p *problems) extract(path, name string, e Extract) {
	validName := extractName.MatchString(name)
	if !validName {
		p.add(path, "invalid extract name %q: must start with a lower-case letter and use only letters, digits and '_', so a template can read .Fields.%s", name, name)
	}
	p.messageField(path+".field", e.Field)
	re := p.regex(path+".regex", e.Regex)
	if re != nil && validName && !slices.Contains(re.SubexpNames(), name) {
		p.add(path+".regex", "must have a named group (?P<%s>...) that supplies the value", name)
	}
}

// thread checks the thread rules against the extracted fields they refer to.
func (p *problems) thread(path string, th *TriggerThread, extract map[string]Extract) {
	if th.By == "" {
		if th.Cooldown.Duration != 0 {
			p.add(path+".cooldown", "requires thread.by")
		}
		if len(th.CloseWhen) > 0 {
			p.add(path+".closeWhen", "requires thread.by")
		}
		return
	}
	p.oneOf(path+".by", string(th.By), string(ThreadByKey))
	if _, ok := extract[extractKey]; th.By == ThreadByKey && !ok {
		p.add(path+".by", "key requires an extract named %q", extractKey)
	}
	if d := th.Cooldown.Duration; d == 0 {
		p.add(path+".cooldown", "required with thread.by")
	} else if d < minThreadCooldown || d > maxThreadCooldown {
		p.add(path+".cooldown", "%s is out of range, must be between %s and %s", d, minThreadCooldown, maxThreadCooldown)
	}
	for _, name := range slices.Sorted(maps.Keys(th.CloseWhen)) {
		at := path + ".closeWhen." + name
		if _, ok := extract[name]; !ok {
			p.add(at, "not an extracted field")
		}
		p.required(at, th.CloseWhen[name])
	}
}
