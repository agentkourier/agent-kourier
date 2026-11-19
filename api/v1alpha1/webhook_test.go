package v1alpha1

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hook loads the webhook connection fixture and changes its spec.webhook with f.
func hook(f func(*WebhookSpec)) func(*ChatConnection) {
	return func(o *ChatConnection) { f(o.Spec.Webhook) }
}

func TestWebhookConnectionValidate(t *testing.T) {
	const at = "spec.webhook"
	credential := at + ".fields[0].header: reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)"
	runMutations[ChatConnection](t, "webhook/connection.yaml", []mutation[ChatConnection]{
		{"the fixture ok", func(*ChatConnection) {}, ""},
		{"the token scheme ok", hook(func(w *WebhookSpec) {
			w.Verify = WebhookVerify{Scheme: WebhookToken, Header: "X-Automation-Token"}
		}), ""},
		{"a prefix with a space ok", hook(func(w *WebhookSpec) {
			w.Verify = WebhookVerify{Scheme: WebhookToken, Header: "X-Token", Prefix: "Bearer "}
		}), ""},
		{"a body timestamp ok", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "/webhookTimestamp", Unit: WebhookUnitMillis, Window: metav1.Duration{Duration: time.Minute}}
		}), ""},
		{"a timestamp window at the cap ok", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "/t", Unit: WebhookUnitSeconds, Window: metav1.Duration{Duration: MaxWebhookTimestampWindow}}
		}), ""},
		{"a fixed channel ok", hook(func(w *WebhookSpec) { w.Channel = WebhookChannel{Value: "OPS"} }), ""},
		{"no actors, and no identity, ok", hook(func(w *WebhookSpec) { w.Actor = WebhookActor{None: true}; w.Identity = "" }), ""},
		{"keep an event with no actor ok", hook(func(w *WebhookSpec) { w.Actor.WhenMissing = WebhookMissingKeep }), ""},
		{"an Atlassian account ID ok", hook(func(w *WebhookSpec) { w.Identity = "712020:00000000-0000-0000-0000-00000000a0e1" }), ""},
		{"a Bitbucket UUID ok", hook(func(w *WebhookSpec) { w.Identity = "{00000000-0000-0000-0000-00000000b0b0}" }), ""},
		{"no fields and no thread ok", hook(func(w *WebhookSpec) { w.Fields, w.Thread = nil, nil }), ""},
		{"no delivery header ok", hook(func(w *WebhookSpec) { w.Delivery = nil }), ""},
		{"an escaped pointer ok", hook(func(w *WebhookSpec) { w.Actor.From = []string{"/a~1b/c~0d", "/"} }), ""},

		{"no webhook block", func(o *ChatConnection) { o.Spec.Webhook = nil }, at + ": is required on a webhook connection"},
		{"no identity", hook(func(w *WebhookSpec) { w.Identity = "" }),
			at + ".identity: is required unless actor.none is true: the agent's own events are dropped by it"},
		{"an identity with actor none", hook(func(w *WebhookSpec) { w.Actor = WebhookActor{None: true} }),
			at + ".identity: is set, but actor.none turns the self-drop off, which alone reads it"},
		{"an identity with a space", hook(func(w *WebhookSpec) { w.Identity = "pr reviewer" }),
			at + `.identity: invalid identity "pr reviewer": use the account as the source names it in the actor pointers: letters, digits and _.:@{}[]/-`},
		{"an identity over the cap", hook(func(w *WebhookSpec) { w.Identity = strings.Repeat("a", MaxWebhookIdentityLen+1) }),
			at + ".identity: is 129 bytes, longer than the 128 byte limit"},
		{"an identity at the cap ok", hook(func(w *WebhookSpec) { w.Identity = strings.Repeat("a", MaxWebhookIdentityLen) }), ""},

		{"no scheme", hook(func(w *WebhookSpec) { w.Verify.Scheme = "" }), at + `.verify.scheme: unsupported value "", must be one of hmac-sha256, token`},
		{"a scheme in the wrong case", hook(func(w *WebhookSpec) { w.Verify.Scheme = "HMAC-SHA256" }),
			at + `.verify.scheme: unsupported value "HMAC-SHA256", must be one of hmac-sha256, token`},
		{"no header", hook(func(w *WebhookSpec) { w.Verify.Header = "" }), at + ".verify.header: required"},
		{"a header with a space", hook(func(w *WebhookSpec) { w.Verify.Header = "X Sig" }),
			at + `.verify.header: invalid header name "X Sig": letters, digits and !#$%&'*+-.^_` + "`" + `|~`},
		{"a header with a colon", hook(func(w *WebhookSpec) { w.Verify.Header = "X-Sig:" }),
			at + `.verify.header: invalid header name "X-Sig:": letters, digits and !#$%&'*+-.^_` + "`" + `|~`},
		{"a header over the cap", hook(func(w *WebhookSpec) { w.Verify.Header = strings.Repeat("X", maxWebhookHeaderNameLen+1) }),
			at + ".verify.header: is 65 bytes, longer than the 64 byte limit"},
		{"a header at the cap ok", hook(func(w *WebhookSpec) { w.Verify.Header = strings.Repeat("X", maxWebhookHeaderNameLen) }), ""},
		{"a prefix over the cap", hook(func(w *WebhookSpec) { w.Verify.Prefix = strings.Repeat("p", MaxWebhookPrefixLen+1) }),
			at + ".verify.prefix: is 33 bytes, longer than the 32 byte limit"},
		{"a prefix with a control character", hook(func(w *WebhookSpec) { w.Verify.Prefix = "sha256=\n" }),
			at + ".verify.prefix: must be printable ASCII: a header value holds no other"},
		{"a timestamp on the token scheme", hook(func(w *WebhookSpec) {
			w.Verify = WebhookVerify{Scheme: WebhookToken, Header: "X-Token",
				Timestamp: &WebhookTimestamp{From: "/t", Unit: WebhookUnitSeconds, Window: metav1.Duration{Duration: time.Minute}}}
		}), at + ".verify.timestamp: is taken by the hmac-sha256 scheme alone: the time is read after the signature passes"},
		{"a timestamp window over the cap", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "/t", Unit: WebhookUnitSeconds, Window: metav1.Duration{Duration: MaxWebhookTimestampWindow + time.Second}}
		}), at + ".verify.timestamp.window: 10m1s is out of range, must be above 0s and at most 10m0s"},
		{"no timestamp window", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "/t", Unit: WebhookUnitSeconds}
		}), at + ".verify.timestamp.window: 0s is out of range, must be above 0s and at most 10m0s"},
		{"a timestamp of no unit", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "/t", Window: metav1.Duration{Duration: time.Minute}}
		}), at + `.verify.timestamp.unit: unsupported value "", must be one of ms, s`},
		{"a timestamp at the root", hook(func(w *WebhookSpec) {
			w.Verify.Timestamp = &WebhookTimestamp{From: "", Unit: WebhookUnitSeconds, Window: metav1.Duration{Duration: time.Minute}}
		}), at + ".verify.timestamp.from: must be a JSON Pointer that is not the root, at most 256 bytes"},

		{"a channel from a pointer and a value", hook(func(w *WebhookSpec) { w.Channel.Value = "acme/widgets" }), at + ".channel: set from or value, and not both"},
		{"a channel from neither", hook(func(w *WebhookSpec) { w.Channel = WebhookChannel{} }), at + ".channel: set from or value, and not both"},
		{"a fixed channel with a colon", hook(func(w *WebhookSpec) { w.Channel = WebhookChannel{Value: "a:b"} }),
			at + `.channel.value: invalid channel "a:b": letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts`},
		{"a fixed channel over the cap", hook(func(w *WebhookSpec) { w.Channel = WebhookChannel{Value: strings.Repeat("a", maxWebhookChannelLen+1)} }),
			at + ".channel.value: is 141 bytes, longer than the 140 byte limit"},
		{"a pointer without its slash", hook(func(w *WebhookSpec) { w.Channel.From = []string{"repository/full_name"} }),
			at + ".channel.from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},
		{"a pointer over the cap", hook(func(w *WebhookSpec) { w.Channel.From = []string{"/" + strings.Repeat("a", MaxWebhookPointerLen)} }),
			at + ".channel.from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},
		{"a pointer at the cap ok", hook(func(w *WebhookSpec) { w.Channel.From = []string{"/" + strings.Repeat("a", MaxWebhookPointerLen-1)} }), ""},
		{"too many pointers", hook(func(w *WebhookSpec) { w.Channel.From = slices.Repeat([]string{"/a"}, MaxWebhookFrom+1) }),
			at + ".channel.from: has 9 items, maximum is 8"},
		{"the most pointers ok", hook(func(w *WebhookSpec) { w.Channel.From = slices.Repeat([]string{"/a"}, MaxWebhookFrom) }), ""},
		{"the root as a pointer", hook(func(w *WebhookSpec) { w.Actor.From = []string{""} }),
			at + ".actor.from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},
		{"a bad escape in a pointer", hook(func(w *WebhookSpec) { w.Actor.From = []string{"/a~2b"} }),
			at + ".actor.from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},
		{"a trailing escape in a pointer", hook(func(w *WebhookSpec) { w.Actor.From = []string{"/a~"} }),
			at + ".actor.from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},

		{"no actor", hook(func(w *WebhookSpec) { w.Actor.From = nil }), at + ".actor.from: is required unless actor.none is true"},
		{"actor none with pointers", hook(func(w *WebhookSpec) { w.Actor.None, w.Identity = true, "" }),
			at + ".actor.from: is set, but actor.none turns the self-drop off"},
		{"actor none with whenMissing", hook(func(w *WebhookSpec) {
			w.Actor = WebhookActor{None: true, WhenMissing: WebhookMissingDrop}
			w.Identity = ""
		}),
			at + ".actor.whenMissing: is set, but actor.none turns the self-drop off"},
		{"an unknown whenMissing", hook(func(w *WebhookSpec) { w.Actor.WhenMissing = "skip" }),
			at + `.actor.whenMissing: unsupported value "skip", must be one of drop, keep`},

		{"a field read from the signature's header", hook(func(w *WebhookSpec) { w.Fields[0].Header = "x-hub-signature-256" }), credential},
		{"a field read from a cookie", hook(func(w *WebhookSpec) { w.Fields[0].Header = "Cookie" }), credential},
		{"a field read from authorization", hook(func(w *WebhookSpec) { w.Fields[0].Header = "AUTHORIZATION" }), credential},
		{"a field read from a header that is no header", hook(func(w *WebhookSpec) { w.Fields[0].Header = "X Event" }),
			at + `.fields[0].header: invalid header name "X Event": letters, digits and !#$%&'*+-.^_` + "`" + `|~`},
		{"a field with a pointer and a header", hook(func(w *WebhookSpec) { w.Fields[0].From = []string{"/x"} }), at + ".fields[0]: set from or header, and not both"},
		{"a field with neither", hook(func(w *WebhookSpec) { w.Fields[0].Header = "" }), at + ".fields[0]: set from or header, and not both"},
		{"a field with a bad pointer", hook(func(w *WebhookSpec) { w.Fields[1].From = []string{"action"} }),
			at + ".fields[1].from[0]: must be a JSON Pointer that is not the root, at most 256 bytes"},
		{"a duplicate field", hook(func(w *WebhookSpec) { w.Fields[3].Name = "action" }), at + `.fields[3].name: duplicate of index 1`},
		{"a reserved field name", hook(func(w *WebhookSpec) { w.Fields[3].Name = "key" }),
			at + `.fields[3].name: "key" is reserved: the prompt view gives it a meaning`},
		{"the other reserved field name", hook(func(w *WebhookSpec) { w.Fields[3].Name = "state" }),
			at + `.fields[3].name: "state" is reserved: the prompt view gives it a meaning`},
		{"a field name with an underscore", hook(func(w *WebhookSpec) { w.Fields[3].Name = "head_sha" }),
			at + `.fields[3].name: invalid name "head_sha": a lower-case letter, then letters and digits`},
		{"a field name over the cap", hook(func(w *WebhookSpec) { w.Fields[3].Name = strings.Repeat("a", maxWebhookFieldNameLen+1) }),
			at + ".fields[3].name: is 64 bytes, longer than the 63 byte limit"},
		{"no field name", hook(func(w *WebhookSpec) { w.Fields[3].Name = "" }), at + ".fields[3].name: required"},
		{"a field of an unknown kind", hook(func(w *WebhookSpec) { w.Fields[3].Kind = "number" }),
			at + `.fields[3].kind: unsupported value "number", must be one of id, text`},
		{"too many fields", hook(func(w *WebhookSpec) { w.Fields = webhookFields(MaxWebhookFields + 1); w.Thread = nil }),
			at + ".fields: has 33 items, maximum is 32"},
		{"the most fields ok", hook(func(w *WebhookSpec) { w.Fields = webhookFields(MaxWebhookFields); w.Thread.Field = "f0" }), ""},

		{"a thread on a text field", hook(func(w *WebhookSpec) { w.Thread.Field = "title" }), at + `.thread.field: "title" is not an id field`},
		{"a thread on no field", hook(func(w *WebhookSpec) { w.Thread.Field = "pr" }), at + `.thread.field: "pr" is not a field of the connection`},
		{"a thread prefix with a colon", hook(func(w *WebhookSpec) { w.Thread.Prefix = "pr:" }),
			at + `.thread.prefix: invalid prefix "pr:": letters, digits, '.', '_' and '-'`},
		{"a thread prefix over the cap", hook(func(w *WebhookSpec) { w.Thread.Prefix = strings.Repeat("p", MaxWebhookPrefixLen+1) }),
			at + ".thread.prefix: is 33 bytes, longer than the 32 byte limit"},
		{"a thread with no prefix ok", hook(func(w *WebhookSpec) { w.Thread.Prefix = "" }), ""},

		{"no delivery header", hook(func(w *WebhookSpec) { w.Delivery.Header = "" }), at + ".delivery.header: required"},
		{"a delivery header that is no header", hook(func(w *WebhookSpec) { w.Delivery.Header = "X Delivery" }),
			at + `.delivery.header: invalid header name "X Delivery": letters, digits and !#$%&'*+-.^_` + "`" + `|~`},
		{"a delivery header over the cap", hook(func(w *WebhookSpec) { w.Delivery.Header = strings.Repeat("X", maxWebhookHeaderNameLen+1) }),
			at + ".delivery.header: is 65 bytes, longer than the 64 byte limit"},
		{"a delivery header that is the signature's", hook(func(w *WebhookSpec) { w.Delivery.Header = "X-HUB-SIGNATURE-256" }),
			at + ".delivery.header: reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)"},
		{"a delivery header that is a cookie", hook(func(w *WebhookSpec) { w.Delivery.Header = "cookie" }),
			at + ".delivery.header: reads a credential header (verify.header, Authorization, Proxy-Authorization or Cookie)"},
	})
}

// webhookFields returns n id fields with distinct names, each read from a header.
func webhookFields(n int) []WebhookField {
	out := make([]WebhookField, n)
	for i := range out {
		out[i] = WebhookField{Name: fmt.Sprintf("f%d", i), Header: fmt.Sprintf("X-F%d", i), Kind: WebhookKindID}
	}
	return out
}

// A webhook block is a webhook connection's alone, and one on another platform is checked too, so that no other platform's connection
// carries a webhook's settings unchecked.
func TestAWebhookBlockOnAnotherPlatform(t *testing.T) {
	c := load[ChatConnection](t, "webhook/connection.yaml")
	c.Spec.Platform = PlatformSlack
	c.Spec.Webhook.Verify.Scheme = ""
	want := []string{"spec.webhook: is set, but only a webhook connection has one",
		`spec.webhook.verify.scheme: unsupported value "", must be one of hmac-sha256, token`}
	if got := problemsOf(c.Validate()); !slices.Equal(got, want) {
		t.Errorf("Validate() problems = %q, want %q", got, want)
	}
}

func TestWebhookPointers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"/a", true}, {"/", true}, {"//", true}, {"/a/0/b", true}, {"/a~0b", true}, {"/a~1b", true}, {"/é", true},
		{"/" + strings.Repeat("a", MaxWebhookPointerLen-1), true},
		{"", false}, {"a", false}, {"a/b", false}, {"/a~", false}, {"/a~2", false}, {"/~a", false},
		{"/" + strings.Repeat("a", MaxWebhookPointerLen), false},
	} {
		if got := ValidWebhookPointer(tc.in); got != tc.want {
			t.Errorf("ValidWebhookPointer(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestWebhookEventTriggerValidate(t *testing.T) {
	const at = "spec.chat.triggers[0]"
	set := func(f func(*Trigger)) func(*Binding) {
		return func(o *Binding) { f(&o.Spec.Chat.Triggers[0]) }
	}
	runMutations[Binding](t, "webhook/binding.yaml", []mutation[Binding]{
		{"the fixture ok", func(*Binding) {}, ""},
		{"no match ok", set(func(t *Trigger) { t.Match = nil }), ""},
		{"an exclude ok", set(func(t *Trigger) { t.Exclude = map[MessageField]string{"draft": "^true$"} }), ""},
		{"a template with no fields ok", set(func(t *Trigger) { t.PromptTemplate = "Look at {{ .Channel }}." }), ""},
		{"the most match entries ok", set(func(t *Trigger) { t.Match = webhookMatch(len(messageFields())) }), ""},
		{"no rate limit", set(func(t *Trigger) { t.Limits.RateLimit = nil }),
			at + ".limits.rateLimit: required: set maxRuns, per and maxConcurrent; " + webhookLimitsHint},
		{"no daily cap", set(func(t *Trigger) { t.Limits.MaxRunsPerDay = nil }),
			at + ".limits.maxRunsPerDay: required: set the cap for a day, or 0 for no cap; " + webhookLimitsHint},
		{"no template", set(func(t *Trigger) { t.PromptTemplate = "" }), at + ".promptTemplate: required"},
		{"a template that loops", set(func(t *Trigger) { t.PromptTemplate = "{{ range .Fields }}x{{ end }}" }),
			at + ".promptTemplate: invalid template: promptTemplate:1:9: range is not allowed: a prompt template has no loops"},
		{"a template that will not run", set(func(t *Trigger) { t.PromptTemplate = "{{ slice .Fields.number 0 100 }}" }),
			at + `.promptTemplate: invalid template: template: promptTemplate:1:3: executing "promptTemplate" at <slice .Fields.number 0 100>: error calling slice: index out of range: 100`},
		{"a template over the cap", set(func(t *Trigger) { t.PromptTemplate = strings.Repeat("a", maxPromptTemplateLen+1) }),
			at + ".promptTemplate: is 8193 bytes, longer than the 8192 byte limit"},
		{"a sender", set(func(t *Trigger) { t.From = &BotSource{ID: "B0123456789", Kind: SenderKindBot} }),
			at + ".from: not allowed with type: webhookEvent"},
		{"an extract", set(func(t *Trigger) {
			t.Extract = map[string]Extract{"key": {Field: FieldText, Regex: "(?P<key>.+)"}}
		}), at + ".extract: not allowed with type: webhookEvent"},
		{"a thread", set(func(t *Trigger) { t.Thread = TriggerThread{By: ThreadByKey} }), at + ".thread: not allowed with type: webhookEvent"},
		{"a field name with an underscore", set(func(t *Trigger) { t.Exclude = map[MessageField]string{"head_sha": "."} }),
			at + `.exclude.head_sha: invalid field name "head_sha": a lower-case letter, then letters and digits`},
		{"a field name over the cap", set(func(t *Trigger) { t.Match = map[MessageField]string{MessageField(strings.Repeat("a", 64)): "."} }),
			at + ".match: entry 0, in name order, is named with 64 bytes, longer than the 63 byte limit"},
		{"an empty regex", set(func(t *Trigger) { t.Match["event"] = "" }), at + ".match.event: required"},
		{"a regex that does not parse", set(func(t *Trigger) { t.Match["event"] = "(" }),
			at + ".match.event: invalid regular expression: error parsing regexp: missing closing ): `(`"},
		{"too many match entries", set(func(t *Trigger) { t.Match = webhookMatch(len(messageFields()) + 1) }),
			at + ".match: has 7 entries, maximum is 6"},
	})
}

// webhookMatch returns n match entries with distinct field names.
func webhookMatch(n int) map[MessageField]string {
	m := map[MessageField]string{}
	for i := range n {
		m[MessageField(fmt.Sprintf("f%d", i))] = "."
	}
	return m
}

// The channel and the output of a Binding whose triggers are all webhookEvent take the webhook's shapes, and those of any other Binding
// Slack's, as before: a Slack Binding is refused output none and a webhook channel as it was.
func TestWebhookBindingChannelAndOutput(t *testing.T) {
	slackChannel := `spec.chat.channel: invalid channel "acme/widgets": must be a channel ID such as C0123456789, not a name`
	runMutations[Binding](t, "webhook/binding.yaml", []mutation[Binding]{
		{"a project key ok", func(o *Binding) { o.Spec.Chat.Channel = "OPS" }, ""},
		{"a Slack channel ID ok", func(o *Binding) { o.Spec.Chat.Channel = "C0123456789" }, ""},
		{"a channel at the webhook cap ok", func(o *Binding) { o.Spec.Chat.Channel = strings.Repeat("a", maxWebhookChannelLen) }, ""},
		{"a channel over the webhook cap", func(o *Binding) { o.Spec.Chat.Channel = strings.Repeat("a", maxWebhookChannelLen+1) },
			"spec.chat.channel: is 141 bytes, longer than the 140 byte limit"},
		{"a channel with a colon", func(o *Binding) { o.Spec.Chat.Channel = "a:b" },
			`spec.chat.channel: invalid channel "a:b": letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts`},
		{"a channel with an empty part", func(o *Binding) { o.Spec.Chat.Channel = "acme//widgets" },
			`spec.chat.channel: invalid channel "acme//widgets": letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts`},
		{"output live is left to the platform", func(o *Binding) { o.Spec.Chat.Output = OutputLive }, ""},
		{"output final is left to the platform", func(o *Binding) { o.Spec.Chat.Output = OutputFinal }, ""},
		{"an unknown output", func(o *Binding) { o.Spec.Chat.Output = "None" },
			`spec.chat.output: unsupported value "None", must be one of live, final, none`},
		{"a mention trigger beside the webhookEvent trigger: Slack's channel", func(o *Binding) {
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, Trigger{Type: TriggerMention})
			o.Spec.Chat.Output = OutputLive
		}, slackChannel},
		{"no triggers: Slack's channel", func(o *Binding) {
			o.Spec.Chat.Triggers = nil
			o.Spec.Chat.Output = OutputLive
			o.Default()
		}, slackChannel},
		{"a mention trigger beside the webhookEvent trigger: Slack's outputs", func(o *Binding) {
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, Trigger{Type: TriggerMention})
			o.Spec.Chat.Channel = "C0123456789"
		}, `spec.chat.output: unsupported value "none", must be one of live, final`},
	})
}

// What a webhook connection's Bindings may name and use, and what the platform serves.
func TestTheWebhookPlatform(t *testing.T) {
	c := load[ChatConnection](t, "webhook/connection.yaml")
	p := platformOf(t, c.Spec)
	if p.Name != "webhook" || p.ActorName != "webhook" || p.Replies || !p.Webhook || p.AskUser || p.ToolApprovals || p.Feedback {
		t.Errorf("the webhook platform is %+v; want no replies, no buttons, webhookEvent triggers", p)
	}
	if !p.Channel.Taken() || p.Sender.Taken() || p.User.Taken() || p.Group.Taken() {
		t.Errorf("the webhook platform takes %+v; want a channel alone", p)
	}
	if !reflect.DeepEqual(p.WebhookFields, c.Spec.Webhook.Fields) {
		t.Errorf("WebhookFields = %+v, want the connection's %+v", p.WebhookFields, c.Spec.Webhook.Fields)
	}
	p.WebhookFields[0].Name = "changed"
	if c.Spec.Webhook.Fields[0].Name == "changed" {
		t.Error("WebhookFields shares its array with the connection's spec")
	}
	if _, ok := ChatPlatformOf(ChatConnectionSpec{Platform: PlatformWebhook}); ok {
		t.Error("ChatPlatformOf has rules for a webhook connection with no webhook block, which Validate refuses")
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"acme/widgets", true}, {"OPS", true}, {"C0123456789", true}, {"a.b_c-d/e", true}, {"0", true},
		{strings.Repeat("a", maxWebhookChannelLen), true}, {strings.Repeat("a", maxWebhookChannelLen+1), false},
		{"", false}, {"/acme", false}, {"acme/", false}, {"acme//w", false}, {"-acme", false}, {"a:b", false}, {"a b", false}, {"é", false},
	} {
		if got := p.Channel.Matches(tc.in); got != tc.want {
			t.Errorf("the webhook channel shape Matches(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got := ValidWebhookChannel(tc.in); got != tc.want {
			t.Errorf("ValidWebhookChannel(%q) = %v, want %v, as the platform's channel shape", tc.in, got, tc.want)
		}
	}
}

func TestWebhookPlatformMismatches(t *testing.T) {
	webhook := platformOf(t, load[ChatConnection](t, "webhook/connection.yaml").Spec)
	write := "set it to none: on this platform the agent posts with its own tools and Agent Kourier posts nothing (left out, it is live)"
	onWebhook := "use type webhookEvent: a webhook connection takes no other (a Binding that lists no triggers has [{type: mention}])"
	noField := "name a field of the connection's spec.webhook.fields"
	for _, tc := range []struct {
		name     string
		platform ChatPlatform
		file     string
		mutate   func(*Binding)
		want     []PlatformMismatch
	}{
		{"a webhook Binding on its connection", webhook, "webhook/binding.yaml", func(*Binding) {}, nil},
		{"a Slack channel ID is a webhook channel too", webhook, "webhook/binding.yaml", func(b *Binding) { b.Spec.Chat.Channel = "C0123456789" }, nil},
		{"output live", webhook, "webhook/binding.yaml", func(b *Binding) { b.Spec.Chat.Output = OutputLive }, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"live" is not supported`, Fix: write},
		}},
		{"output left out, as a hand-made spec has it, is live", webhook, "webhook/binding.yaml", func(b *Binding) { b.Spec.Chat.Output = "" }, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"live" is not supported`, Fix: write},
		}},
		{"output final", webhook, "webhook/binding.yaml", func(b *Binding) { b.Spec.Chat.Output = OutputFinal }, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"final" is not supported`, Fix: write},
		}},
		{"a match and an exclude on no field of the connection, and a template that reads a text field and no field", webhook, "webhook/binding.yaml", func(b *Binding) {
			tr := &b.Spec.Chat.Triggers[0]
			tr.Match["merged"] = "."
			tr.Exclude = map[MessageField]string{"draft": "."}
			tr.PromptTemplate = "{{ .Fields.title }} {{ .Fields.number }} {{ .Fields.body }}"
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[0].match.merged", Unsupported: true, Problem: "is not a field of the connection", Fix: noField},
			{Path: "spec.chat.triggers[0].exclude.draft", Unsupported: true, Problem: "is not a field of the connection", Fix: noField},
			{Path: "spec.chat.triggers[0].promptTemplate", Unsupported: true, Problem: "reads .Fields.body, which is not a field of the connection",
				Fix: "read only id fields: a text field reaches the agent as untrusted data alone"},
			{Path: "spec.chat.triggers[0].promptTemplate", Unsupported: true, Problem: "reads .Fields.title, which is not an id field",
				Fix: "read only id fields: a text field reaches the agent as untrusted data alone"},
		}},
		{"a match on a text field ok", webhook, "webhook/binding.yaml", func(b *Binding) { b.Spec.Chat.Triggers[0].Match["title"] = "fix" }, nil},
		{"every other trigger and feature, in the order of the fields", webhook, "binding.yaml", func(b *Binding) {
			b.Spec.Chat.Channel = "acme/widgets"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: "U0123456789"}}
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789"}
		}, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"live" is not supported`, Fix: write},
			{Path: "spec.chat.triggers[0].type", Unsupported: true, Problem: `"mention" is not supported`, Fix: onWebhook},
			{Path: "spec.chat.triggers[1].type", Unsupported: true, Problem: `"botMessage" is not supported`, Fix: onWebhook},
			{Path: "spec.chat.mentions", Unsupported: true, Problem: "mentions are not supported",
				Fix: "remove it: Agent Kourier posts nothing on the platform, so it notifies nobody"},
			{Path: "spec.interactions.askUser", Unsupported: true, Problem: "questions to a person are not supported", Fix: "set it to false"},
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported",
				Fix: "set it to false and remove spec.interactions.approverGroups, which requires it"},
			{Path: "spec.interactions.approverGroups", Unsupported: true, Problem: "approver groups are not supported",
				Fix: "remove it: the platform asks for no tool approvals"},
			{Path: "spec.feedback.enabled", Unsupported: true, Problem: "feedback buttons are not supported", Fix: "set it to false"},
		}},
		{"a webhookEvent trigger on Slack", slack(t), "webhook/binding.yaml", func(b *Binding) {
			b.Spec.Chat.Channel = "C0123456789"
			b.Spec.Chat.Output = OutputLive
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[0].type", Unsupported: true, Problem: `"webhookEvent" is not supported`,
				Fix: "use it on a connection of the webhook platform alone"},
		}},
		{"a webhook Binding on Slack", slack(t), "webhook/binding.yaml", func(*Binding) {}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"acme/widgets" is not a channel ID`,
				Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
			{Path: "spec.chat.output", Unsupported: true, Problem: `"none" is not supported`, Fix: "set it to live or final, or leave it out for live"},
			{Path: "spec.chat.triggers[0].type", Unsupported: true, Problem: `"webhookEvent" is not supported`,
				Fix: "use it on a connection of the webhook platform alone"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := load[Binding](t, tc.file)
			tc.mutate(b)
			if got := b.Spec.PlatformMismatches(tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlatformMismatches =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// The keys of a webhook connection's credentials Secret follow its scheme: the HMAC's secret, or the token.
func TestWebhookCredentialKeys(t *testing.T) {
	c := load[ChatConnection](t, "webhook/connection.yaml")
	if got := CredentialKeys(c.Spec); !slices.Equal(got, []string{"webhookSecret"}) {
		t.Errorf("hmac-sha256: CredentialKeys = %q, want [webhookSecret]", got)
	}
	c.Spec.Webhook.Verify.Scheme = WebhookToken
	if got := CredentialKeys(c.Spec); !slices.Equal(got, []string{"token"}) {
		t.Errorf("token: CredentialKeys = %q, want [token]", got)
	}
	for _, spec := range []ChatConnectionSpec{
		{Platform: PlatformWebhook},
		{Platform: PlatformWebhook, Webhook: &WebhookSpec{Verify: WebhookVerify{Scheme: "hmac-sha1"}}},
	} {
		if got := CredentialKeys(spec); got != nil {
			t.Errorf("%+v: CredentialKeys = %q, want none: Validate refuses the connection", spec, got)
		}
	}
}
