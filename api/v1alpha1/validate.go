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

// channelID matches Slack channel IDs, the only platform's; its literal must equal ChatBinding.Channel's Pattern marker.
var channelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

// reactionName matches Slack's bare reaction names. The platform adds and removes
// the surrounding colons; accepting only names keeps configuration out of the
// platform adapter and prevents an invalid outbox row.
var reactionName = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9+_-]*|[+-][a-z0-9][a-z0-9+_-]*)$`)

// botID matches Slack bot IDs, the only platform's; its literal must equal BotSource.ID's Pattern marker. A second
// platform moves it to a per-platform check at resolution.
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
	// maxApproverGroups keeps each approval press within a bounded number of uncached group lookups.
	maxApproverGroups = 10
	// maxMentions bounds the names a Binding declares, each of which the agent is told of in its first prompt.
	maxMentions = 50
	// maxTriggers, maxExtract, maxHeaders and maxHeaderNameLen bound what the CRD schema's validation rules walk: the
	// apiserver prices a rule by the largest list, map or string it can meet, and refuses a schema whose rules cost
	// too much, so each of these is also a maxItems, maxProperties or maxLength in the schema. They are set far above
	// any real use, which today is two triggers, three extracts, one header and a header name of 16 characters. A config
	// over one is refused when it loads, on the file route too. maxExtract also bounds thread.closeWhen: a closeWhen
	// key must be an extract's, so that bound refuses nothing the extract cap does not.
	maxTriggers      = 50
	maxExtract       = 32
	maxHeaders       = 32
	maxHeaderNameLen = 256
)

// maxPromptTemplateLen, maxAgentNameLen and maxUserIDLen cap the free text of a Binding that nothing else bounds:
// without a cap the apiserver stores a Binding of megabytes, and every broker keeps each one in memory, parses its
// template and runs it. Each is also a maxLength in the schema, so the apiserver refuses a longer one and so does
// a file loaded by the broker. They count bytes, as Validate does, and the schema counts characters, so the schema
// is never the stricter of the two. A template of 8192 bytes is twice what the prompt keeps of its output
// (MaxInstructionUnits in internal/render), a real agent name is a DNS name (253) and a real user ID is a login or
// an e-mail address.
const (
	maxPromptTemplateLen = 8192
	maxAgentNameLen      = 253
	maxUserIDLen         = 256
)

// The caps below bound the strings and the list that nothing else bounded, so that one namespace writer cannot make an
// object as large as the apiserver allows, which every broker would then keep in memory. Each is also a maxItems,
// maxLength or rule in the schema, so the apiserver refuses a longer one and so does a file loaded by the broker. They
// count bytes, as Validate does, and the schema counts characters, so the schema is never the stricter of the two. Each is
// set far above any real use, which today is:
//
//   - maxAllowedNamespaces 256: one namespace in every example (a namespace is at most 63 characters, so the list is at most 16 KiB).
//   - maxChatIDLen 32: a channel, a sender and a user group are 11 characters in every example, and Slack's IDs are 9 to 11
//     (unverified: not readable in this repository). A mention's name and user ID are already bounded by their patterns, to
//     63 and 21: maxMentionNameLen and maxMentionUserLen say so as caps, so that the schema has a maxLength for them and a
//     value over one is refused without being shown.
//   - maxEmojiLen 100: the default reaction, hourglass_flowing_sand, is 21. Slack's own limit on a custom emoji name is
//     100 characters (unverified: not readable in this repository).
//   - maxURLLen 2048: the longest URL in an example or a test fixture is under 100 characters.
//   - maxHeaderValueLen 4096: a static header value in an example is under 50 bytes, and a secret goes in valueSecretRef.
//   - maxExtractNameLen 63, as a mention name: the longest extract name in an example is 9 characters. A closeWhen key
//     must be an extract name, so it is bounded too.
//   - maxCloseWhenValueLen 256: the value in an example is RESOLVED.
const (
	maxAllowedNamespaces = 256
	maxChatIDLen         = 32
	maxMentionNameLen    = 63
	maxMentionUserLen    = 21
	maxEmojiLen          = 100
	maxURLLen            = 2048
	maxHeaderValueLen    = 4096
	maxExtractNameLen    = 63
	maxCloseWhenValueLen = 256
)

// mentionName is what an agent writes after an "@"; its literal must equal ChatMention.Name's Pattern marker. mentionUser
// is a Slack user ID, the only platform's; its literal must equal ChatMention.User's Pattern marker, and matches what the
// Slack adapter makes a mention of.
var (
	mentionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	mentionUser = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
)

// broadcastNames are the words a platform reads as a mention of everyone. None may be declared, so that no reply that names
// one can ever look as if a broadcast were allowed.
var broadcastNames = map[string]bool{"here": true, "channel": true, "everyone": true}

// approverGroupID matches Slack user group IDs. Its literal is also documented by Interactions.ApproverGroups.
var approverGroupID = regexp.MustCompile(`^S[A-Z0-9]+$`)

// IsReservedHeader reports whether name carries or could forge the Binding identity (D14),
// so an AgentBackend must not set it. Dialects refuse it too. The kagent alpha5 headers
// are reserved per D32 (WP-03, WP-20c review): X-Agent-Name selects the acting user via
// X-User-Id or ?user_id=; X-Share-Token grants the session owner's sessions on Agent Kourier's
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

// maxBytes reports v if it is longer than limit bytes.
func (p *problems) maxBytes(path, v string, limit int) {
	if len(v) > limit {
		p.add(path, "is %d bytes, longer than the %d byte limit", len(v), limit)
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

// cappedDuration checks a duration that must be positive and at most limit.
func (p *problems) cappedDuration(path string, d metav1.Duration, limit time.Duration) {
	if d.Duration > limit {
		p.add(path, "%s is out of range, must be at most %s", d.Duration, limit)
		return
	}
	p.positive(path, int64(d.Duration))
}

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

// namespaces checks the namespaces of an allowedNamespaces list. A list over the cap is reported once and its entries not
// checked, so that a list of a million bad entries is one problem and not a million.
func (p *problems) namespaces(path string, ns []string) {
	if len(ns) > maxAllowedNamespaces {
		p.add(path, "has %d items, maximum is %d", len(ns), maxAllowedNamespaces)
		return
	}
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

// backendURL checks the url of an AgentBackend.
func (p *problems) backendURL(raw string) {
	if raw == "" {
		p.add("spec.url", "required")
		return
	}
	if len(raw) > maxURLLen {
		// Not parsed, and no verdict below is given: each shows the host, which is as long as the URL may be.
		p.maxBytes("spec.url", raw, maxURLLen)
		return
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		p.add("spec.url", "must be an http or https URL with a host")
	case u.User != nil:
		// net/http would turn userinfo into Basic auth, bypassing the Binding identity.
		p.add("spec.url", "must not contain credentials; identity comes from the Binding (D14)")
	case (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		// The message may reach a log or a status that more people read than the object, so it shows what is needed to fix
		// the verdict, the scheme and the host, and no more: not a password, a query or a fragment, which a token rides in,
		// and not a path or an opaque part, which may hold one too. The verdict does not depend on what is left out.
		shown := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		p.add("spec.url", "invalid URL %q: must be an http or https URL with a host", shown)
	case strings.ContainsAny(raw, "?#"):
		// Both dialects refuse a query or fragment when they build the endpoint, and the generic one would send a card request
		// to a URL that the query breaks. Any "?" or "#" counts, empty or not, so the rule has a twin in the schema that
		// needs no parse. The message shows neither: a token rides there.
		p.add("spec.url", "must not have a query or fragment")
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
	p.backendURL(s.URL)
	if len(s.Headers) > maxHeaders {
		p.add("spec.headers", "has %d items, maximum is %d", len(s.Headers), maxHeaders)
	}
	for i, h := range s.Headers {
		path := fmt.Sprintf("spec.headers[%d]", i)
		p.required(path+".name", h.Name)
		p.maxBytes(path+".name", h.Name, maxHeaderNameLen)
		p.maxBytes(path+".value", h.Value, maxHeaderValueLen)
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
	p.name("spec.credentialsSecretRef.name", s.CredentialsSecretRef.Name)
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
	p.maxBytes("spec.agent.name", s.Agent.Name, maxAgentNameLen)
	p.namespace("spec.agent.namespace", s.Agent.Namespace)
	if s.Identity.UserID == "" && s.Identity.TokenSecretRef == nil {
		p.add("spec.identity", "set userId, tokenSecretRef, or both")
	}
	p.maxBytes("spec.identity.userId", s.Identity.UserID, maxUserIDLen)
	p.secretRef("spec.identity.tokenSecretRef", s.Identity.TokenSecretRef)
	p.objectRef("spec.chat.connectionRef", &s.Chat.ConnectionRef)
	if c := s.Chat.Channel; c == "" {
		p.add("spec.chat.channel", "required")
	} else if len(c) > maxChatIDLen {
		p.maxBytes("spec.chat.channel", c, maxChatIDLen) // not shown: the channel is in the messages that name a conflict
	} else if !channelID.MatchString(c) {
		p.add("spec.chat.channel", "invalid channel %q: must be a channel ID such as C0123456789, not a name", c)
	}
	p.oneOf("spec.chat.output", string(s.Chat.Output), string(OutputLive), string(OutputFinal))
	p.oneOf("spec.chat.progress", string(s.Chat.Progress), string(ProgressSteps), string(ProgressStepsAndActivity), string(ProgressActivity))
	p.oneOf("spec.chat.threadReplies", string(s.Chat.ThreadReplies), string(ThreadRepliesAll), string(ThreadRepliesMention))
	p.oneOf("spec.chat.botMentions", string(s.Chat.BotMentions), string(BotMentionsAny), string(BotMentionsNone))
	if a := s.Chat.Acknowledgment; a == nil {
		p.add("spec.chat.acknowledgment", "required after defaulting")
	} else {
		if a.Enabled == nil {
			p.add("spec.chat.acknowledgment.enabled", "required after defaulting")
		}
		if len(a.Emoji) > maxEmojiLen {
			p.maxBytes("spec.chat.acknowledgment.emoji", a.Emoji, maxEmojiLen)
		} else if a.Emoji != "" && !reactionName.MatchString(a.Emoji) {
			p.add("spec.chat.acknowledgment.emoji", "invalid reaction name %q: use lowercase letters, digits, '-', '_' or '+', with '+1' and '-1' allowed", a.Emoji)
		}
	}
	p.triggers("spec.chat.triggers", s.Chat.Triggers)
	p.mentions("spec.chat.mentions", s.Chat.Mentions)
	if n := s.Chat.MentionCap(); n < 1 || n > 10 {
		p.add("spec.chat.maxMentionsPerTurn", "%d is out of range, must be from 1 to 10", n)
	}
	p.interactions("spec.interactions", s.Interactions)
	p.cappedDuration("spec.interactions.timeout", s.Interactions.Timeout, MaxInteractionTimeout)
	p.cappedDuration("spec.session.threadTTL", s.Session.ThreadTTL, MaxThreadTTL)
	return errors.Join(p...)
}

func (p *problems) mentions(path string, mentions []ChatMention) {
	if len(mentions) > maxMentions {
		p.add(path, "has %d items, maximum is %d", len(mentions), maxMentions)
	}
	seen := make(map[string]int, len(mentions))
	for idx, m := range mentions {
		at := fmt.Sprintf("%s[%d]", path, idx)
		switch {
		case m.Name == "":
			p.add(at+".name", "required")
		case broadcastNames[m.Name]:
			p.add(at+".name", "%q is reserved: a broadcast cannot be declared", m.Name)
		case len(m.Name) > maxMentionNameLen:
			p.maxBytes(at+".name", m.Name, maxMentionNameLen)
		case !mentionName.MatchString(m.Name):
			p.add(at+".name", "invalid name %q: use lowercase letters, digits and '-', starting with a letter, at most 63 characters", m.Name)
		}
		if previous, ok := seen[m.Name]; ok && m.Name != "" {
			p.add(at+".name", "duplicate of index %d", previous)
		} else {
			seen[m.Name] = idx
		}
		switch {
		case m.User == "":
			p.add(at+".user", "required")
		case len(m.User) > maxMentionUserLen:
			p.maxBytes(at+".user", m.User, maxMentionUserLen)
		case !mentionUser.MatchString(m.User):
			p.add(at+".user", "invalid user ID %q: must be a user ID such as U0123456789", m.User)
		}
	}
}

func (p *problems) interactions(path string, in Interactions) {
	if len(in.ApproverGroups) > 0 && !in.ToolApprovals {
		p.add(path+".approverGroups", "requires spec.interactions.toolApprovals: true")
	}
	if len(in.ApproverGroups) > maxApproverGroups {
		p.add(path+".approverGroups", "has %d items, maximum is %d", len(in.ApproverGroups), maxApproverGroups)
	}
	seen := make(map[string]int, len(in.ApproverGroups))
	for idx, group := range in.ApproverGroups {
		at := fmt.Sprintf("%s.approverGroups[%d]", path, idx)
		switch {
		case group == "":
			p.add(at, "must not be empty")
		case len(group) > maxChatIDLen:
			p.maxBytes(at, group, maxChatIDLen)
		case !approverGroupID.MatchString(group):
			p.add(at, "invalid group ID %q: must be a Slack user group ID such as S0123456789", group)
		}
		if previous, ok := seen[group]; ok {
			p.add(at, "duplicate of index %d", previous)
		} else {
			seen[group] = idx
		}
	}
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
// that could never be reached.
func (p *problems) triggers(path string, ts []Trigger) {
	if len(ts) > maxTriggers {
		p.add(path, "has %d items, maximum is %d", len(ts), maxTriggers)
	}
	seen := map[string]int{}
	for i := range ts {
		at := fmt.Sprintf("%s[%d]", path, i)
		t := &ts[i]
		p.trigger(at, t)
		// A trigger with the same bot, match, exclude and extract as an earlier one matches the
		// same messages, so the later one would never run. Extract counts because an extract
		// that does not match means the trigger does not match.
		var bot string
		if t.From != nil {
			bot = t.From.ID + "/" + string(t.From.Kind)
		}
		id := fmt.Sprintf("%q %q %q %q %q", t.Type, bot, t.Match, t.Exclude, t.Extract)
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
		{"from", t.From != nil}, {"match", len(t.Match) > 0},
		{"exclude", len(t.Exclude) > 0}, {"extract", len(t.Extract) > 0},
		{"thread", t.Thread.By != "" || t.Thread.Cooldown.Duration != 0 || t.Thread.MaxIdle.Duration != 0 || len(t.Thread.CloseWhen) > 0},
		{"promptTemplate", t.PromptTemplate != ""},
		{"limits", !t.Limits.IsZero()},
	} {
		if f.set {
			p.add(path+"."+f.name, "not allowed with type: mention")
		}
	}
}

func (p *problems) botMessageTrigger(path string, t *Trigger) {
	switch {
	case t.From == nil || t.From.ID == "":
		p.add(path+".from.id", "required")
	case len(t.From.ID) > maxChatIDLen:
		p.maxBytes(path+".from.id", t.From.ID, maxChatIDLen)
	case !botID.MatchString(t.From.ID):
		p.add(path+".from.id", "invalid sender ID %q: must be the sender's ID (Slack: a bot ID such as B0123456789), not a name", t.From.ID)
	}
	if t.From != nil {
		if t.From.Kind == "" {
			p.add(path+".from.kind", "required after defaulting")
		} else {
			p.oneOf(path+".from.kind", string(t.From.Kind), string(SenderKindBot))
		}
	}
	p.fieldRegexes(path+".match", t.Match)
	p.fieldRegexes(path+".exclude", t.Exclude)
	if len(t.Extract) > maxExtract {
		p.add(path+".extract", "has %d entries, maximum is %d", len(t.Extract), maxExtract)
	}
	for _, name := range slices.Sorted(maps.Keys(t.Extract)) {
		if p.longName(path+".extract", name) {
			continue
		}
		p.extract(path+".extract."+name, name, t.Extract[name])
	}
	p.thread(path+".thread", &t.Thread, t.Extract)
	p.limits(path+".limits", t.Limits)
	if t.PromptTemplate == "" {
		p.add(path+".promptTemplate", "required")
	} else if len(t.PromptTemplate) > maxPromptTemplateLen {
		p.maxBytes(path+".promptTemplate", t.PromptTemplate, maxPromptTemplateLen) // refused unparsed
	} else if err := checkPromptTemplate("promptTemplate", t.PromptTemplate, t.Extract); err != nil {
		p.add(path+".promptTemplate", "invalid template: %v", err)
	}
}

// longName reports whether the name of an entry of a map is over the cap of an extract name, and if it is, adds a problem
// that does not show it: a path has the name in it, and so would every message below it.
func (p *problems) longName(path, name string) bool {
	if len(name) <= maxExtractNameLen {
		return false
	}
	p.add(path, "has an entry named with %d bytes, longer than the %d byte limit", len(name), maxExtractNameLen)
	return true
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

// limitsHint points a botMessage trigger that lacks limits at the documented example, which sets both.
const limitsHint = "see the example in how-to/investigate-alertmanager-alerts (https://agentkourier.dev/how-to/investigate-alertmanager-alerts/)"

// limits checks the limits of a botMessage trigger: it must set a rate limit and a daily cap, and a rate limit
// set in part fails on each field it lacks. The window is bounded on both sides: see MinRateWindow and
// MaxLimitWindow. A cap of 0 means none, and it must be written: leaving the cap out is refused.
func (p *problems) limits(path string, l TriggerLimits) {
	if l.RateLimit == nil {
		p.add(path+".rateLimit", "required: set maxRuns, per and maxConcurrent; %s", limitsHint)
	}
	if l.MaxRunsPerDay == nil {
		p.add(path+".maxRunsPerDay", "required: set the cap for a day, or 0 for no cap; %s", limitsHint)
	}
	if r := l.RateLimit; r != nil {
		p.positive(path+".rateLimit.maxRuns", int64(r.MaxRuns))
		p.positive(path+".rateLimit.maxConcurrent", int64(r.MaxConcurrent))
		if d := r.Per.Duration; d < MinRateWindow || d > MaxLimitWindow {
			p.add(path+".rateLimit.per", "%s is out of range, must be between %s and %s", d, MinRateWindow, MaxLimitWindow)
		}
	}
	if n := l.MaxRunsPerDay; n != nil && *n < 0 {
		p.add(path+".maxRunsPerDay", "must not be negative; 0 means no cap")
	}
}

// thread checks the thread rules against the extracted fields they refer to.
func (p *problems) thread(path string, th *TriggerThread, extract map[string]Extract) {
	if th.By == "" {
		if th.Cooldown.Duration != 0 {
			p.add(path+".cooldown", "requires thread.by")
		}
		if th.MaxIdle.Duration != 0 {
			p.add(path+".maxIdle", "requires thread.by")
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
	if d := th.MaxIdle.Duration; d != 0 {
		if floor := 2 * th.Cooldown.Duration; d < floor || d > MaxThreadMaxIdle {
			p.add(path+".maxIdle", "%s is out of range, must be between twice the cooldown (%s) and %s", d, floor, MaxThreadMaxIdle)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(th.CloseWhen)) {
		if p.longName(path+".closeWhen", name) {
			continue
		}
		at := path + ".closeWhen." + name
		if _, ok := extract[name]; !ok {
			p.add(at, "not an extracted field")
		}
		p.required(at, th.CloseWhen[name])
		p.maxBytes(at, th.CloseWhen[name], maxCloseWhenValueLen)
	}
}
