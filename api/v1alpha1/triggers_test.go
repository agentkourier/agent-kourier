package v1alpha1

import (
	"maps"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// message is a chat message as the triggers see it: the neutral fields by name.
type message map[MessageField]string

// passesFilters reports whether m clears a defaulted trigger's match and exclude entries the way
// the intake will (CT-2): every match regex matches and no exclude regex does.
func passesFilters(tr Trigger, m message) bool {
	for f, expr := range tr.Match {
		if !regexp.MustCompile(expr).MatchString(m[f]) {
			return false
		}
	}
	for f, expr := range tr.Exclude {
		if regexp.MustCompile(expr).MatchString(m[f]) {
			return false
		}
	}
	return true
}

// evaluate applies a defaulted botMessage trigger to m: it must clear the filters and every
// extract regex must match; it returns the extracted values. It lives here, not in the
// package, because matching is runtime behaviour.
func evaluate(tr Trigger, m message) (map[string]string, bool) {
	if !passesFilters(tr, m) {
		return nil, false
	}
	got := map[string]string{}
	for name, e := range tr.Extract {
		re := regexp.MustCompile(e.Regex)
		sub := re.FindStringSubmatch(m[e.Field])
		if sub == nil {
			return nil, false
		}
		got[name] = sub[re.SubexpIndex(name)]
	}
	return got, true
}

// alertmanagerTrigger returns the alertmanager preset applied to a bare trigger.
func alertmanagerTrigger() Trigger {
	tr := Trigger{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}, Preset: PresetAlertmanager}
	tr.applyPreset()
	return tr
}

const slackCriticalLink = "https://alertmanager.example.com/#/alerts?receiver=slack-critical"

func TestAlertmanagerPreset(t *testing.T) {
	tests := []struct {
		name  string
		msg   message
		match bool
		want  map[string]string // state, key, alertname when it matches
	}{
		{
			name: "firing, one alert, from the owner's channel",
			msg: message{
				FieldTitle:     "FIRING (1) - KubeAPIErrorBudgetBurn",
				FieldTitleLink: slackCriticalLink,
				FieldFallback:  "[FIRING:1] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + slackCriticalLink,
				FieldText:      "*KubeAPIErrorBudgetBurn* - critical\nThe API server is burning too much error budget.\n",
			},
			match: true,
			want:  map[string]string{"state": "FIRING", "key": "KubeAPIErrorBudgetBurn kube-system", "alertname": "KubeAPIErrorBudgetBurn"},
		},
		{
			name: "firing, several alerts share the group, so the key has no count",
			msg: message{
				FieldTitle:    "FIRING (3) - KubePodCrashLooping",
				FieldFallback: "[FIRING:3] KubePodCrashLooping payments (critical kube-state-metrics) | " + slackCriticalLink,
			},
			match: true,
			want:  map[string]string{"state": "FIRING", "key": "KubePodCrashLooping payments", "alertname": "KubePodCrashLooping"},
		},
		{
			name: "resolved",
			msg: message{
				FieldTitle:    "RESOLVED - KubeAPIErrorBudgetBurn",
				FieldFallback: "[RESOLVED] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + slackCriticalLink,
			},
			match: true,
			want:  map[string]string{"state": "RESOLVED", "key": "KubeAPIErrorBudgetBurn kube-system", "alertname": "KubeAPIErrorBudgetBurn"},
		},
		{
			name: "the trial's kind Alertmanager prefixes the title with [kind], firing",
			msg: message{
				FieldTitle:     "[kind] FIRING (1) - KubePodCrashLooping",
				FieldTitleLink: slackCriticalLink + "-kind",
				FieldFallback:  "[FIRING:1] KubePodCrashLooping default (critical kube-state-metrics) | " + slackCriticalLink + "-kind",
			},
			match: true,
			want:  map[string]string{"state": "FIRING", "key": "KubePodCrashLooping default", "alertname": "KubePodCrashLooping"},
		},
		{
			name: "the trial's kind Alertmanager prefixes the title with [kind], resolved",
			msg: message{
				FieldTitle:     "[kind] RESOLVED - KubePodCrashLooping",
				FieldTitleLink: slackCriticalLink + "-kind",
				FieldFallback:  "[RESOLVED] KubePodCrashLooping default (critical kube-state-metrics) | " + slackCriticalLink + "-kind",
			},
			match: true,
			want:  map[string]string{"state": "RESOLVED", "key": "KubePodCrashLooping default", "alertname": "KubePodCrashLooping"},
		},
		{
			name: "a bracketed tag on a Grafana title is still not an Alertmanager title",
			msg: message{
				FieldTitle:    "[kind] FIRING: HighRequestLatency (payments)",
				FieldFallback: "[FIRING:1] HighRequestLatency (payments) | https://grafana.example.com/alerting/list",
			},
		},
		{
			name: "a title tag over 32 bytes is not a tag",
			msg: message{
				FieldTitle:    "[" + strings.Repeat("k", 33) + "] FIRING (1) - KubePodCrashLooping",
				FieldFallback: "[FIRING:1] KubePodCrashLooping default (critical) | " + slackCriticalLink,
			},
		},
		{
			name: "no namespace group label leaves a double space that the key drops",
			msg: message{
				FieldTitle:    "FIRING (1) - NodeFilesystemSpaceFillingUp",
				FieldFallback: "[FIRING:1] NodeFilesystemSpaceFillingUp  (critical node-exporter) | " + slackCriticalLink,
			},
			match: true,
			want:  map[string]string{"state": "FIRING", "key": "NodeFilesystemSpaceFillingUp", "alertname": "NodeFilesystemSpaceFillingUp"},
		},
		{
			name: "all labels are group labels, so there is no parenthesis",
			msg: message{
				FieldTitle:    "FIRING (1) - TargetDown",
				FieldFallback: "[FIRING:1] TargetDown monitoring  | " + slackCriticalLink,
			},
			match: true,
			want:  map[string]string{"state": "FIRING", "key": "TargetDown monitoring", "alertname": "TargetDown"},
		},
		{
			name: "the Watchdog heartbeat, whose fallback is Alertmanager's default",
			msg: message{
				FieldTitle:     "Prometheus heartbeat",
				FieldTitleLink: "https://alertmanager.example.com/#/alerts?receiver=slack-heartbeat",
				FieldFallback:  "[FIRING:1] Watchdog  (none prometheus) | https://alertmanager.example.com/#/alerts?receiver=slack-heartbeat",
				FieldText:      "Watchdog firing — Prometheus + Alertmanager are alive.",
			},
		},
		{
			name: "Grafana alerting, title and fallback",
			msg: message{
				FieldTitle:    "FIRING: HighRequestLatency (payments)",
				FieldFallback: "FIRING: HighRequestLatency (payments)",
				FieldText:     "**Firing**\n\nValue: A=0.92\nLabels:\n - alertname = HighRequestLatency\n",
			},
		},
		{
			name: "Grafana alerting resolved",
			msg: message{
				FieldTitle:    "RESOLVED: HighRequestLatency (payments)",
				FieldFallback: "RESOLVED: HighRequestLatency (payments)",
			},
		},
		{
			name: "Grafana whose fallback looks like Alertmanager's, the title still rules it out",
			msg: message{
				FieldTitle:    "FIRING: HighRequestLatency (payments)",
				FieldFallback: "[FIRING:1] HighRequestLatency (payments) | https://grafana.example.com/alerting/list",
			},
		},
		{
			name: "a title without its fallback has no key",
			msg:  message{FieldTitle: "FIRING (1) - KubeAPIErrorBudgetBurn"},
		},
		{name: "an empty message", msg: message{}},
		{name: "text only", msg: message{FieldText: "FIRING (1) - KubeAPIErrorBudgetBurn"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := alertmanagerTrigger()
			// The match entries alone must already turn away what is not an alert, so the
			// extracts are a second layer, not the only one.
			if !tc.match && passesFilters(tr, tc.msg) {
				t.Error("the preset's match and exclude entries accept this message")
			}
			got, ok := evaluate(tr, tc.msg)
			if ok != tc.match {
				t.Fatalf("matches = %v, want %v (extracted %v)", ok, tc.match, got)
			}
			if tc.match && !maps.Equal(got, tc.want) {
				t.Errorf("extracted %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnchoredReceiverSeparatesKindFromProduction shows why the docs anchor the receiver: the
// production regex must not take the kind receiver's alerts, nor the reverse.
func TestAnchoredReceiverSeparatesKindFromProduction(t *testing.T) {
	alert := func(link string) message {
		return message{
			FieldTitle:     "FIRING (1) - KubePodCrashLooping",
			FieldTitleLink: link,
			FieldFallback:  "[FIRING:1] KubePodCrashLooping default (critical) | " + link,
		}
	}
	prod, kind := alertmanagerTrigger(), alertmanagerTrigger()
	prod.Match[FieldTitleLink] = `receiver=slack-critical$`
	kind.Match[FieldTitleLink] = `receiver=slack-critical-kind$`
	for _, tc := range []struct {
		name    string
		tr      Trigger
		link    string
		matches bool
	}{
		{"production trigger, production alert", prod, slackCriticalLink, true},
		{"production trigger, kind alert", prod, slackCriticalLink + "-kind", false},
		{"kind trigger, kind alert", kind, slackCriticalLink + "-kind", true},
		{"kind trigger, production alert", kind, slackCriticalLink, false},
	} {
		if _, ok := evaluate(tc.tr, alert(tc.link)); ok != tc.matches {
			t.Errorf("%s: matches = %v, want %v", tc.name, ok, tc.matches)
		}
	}
}

func TestAlertmanagerPresetIsValid(t *testing.T) {
	b := load[Binding](t, "binding-minimal.yaml")
	b.Spec.Chat.Triggers = []Trigger{{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}, Preset: PresetAlertmanager}}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatalf("a bare alertmanager trigger is invalid: %v", err)
	}
	tr := b.Spec.Chat.Triggers[0]
	if tr.Thread.By != ThreadByKey || tr.Thread.Cooldown.Duration != 4*time.Hour || tr.Thread.CloseWhen["state"] != "RESOLVED" {
		t.Errorf("preset thread = %+v, want by key, 4h, closeWhen state RESOLVED", tr.Thread)
	}
}

func TestPresetOverridesFieldByField(t *testing.T) {
	preset := alertmanagerTrigger()
	tr := Trigger{
		Type:    TriggerBotMessage,
		From:    &BotSource{BotID: "B0EXAMPLE01"},
		Preset:  PresetAlertmanager,
		Match:   map[MessageField]string{FieldTitleLink: "receiver=slack-critical-kind", FieldTitle: "^custom$"},
		Exclude: map[MessageField]string{FieldTitle: "^Prometheus heartbeat$"},
		Extract: map[string]Extract{"key": {Field: FieldFooter, Regex: `courier-key: (?P<key>\S+)`}},
		Thread: TriggerThread{
			Cooldown:  metav1.Duration{Duration: time.Hour},
			CloseWhen: map[string]string{"state": "ENDED"},
		},
		PromptTemplate: "Look at {{ .Key }}.",
	}
	tr.applyPreset()

	// What the Binding set wins.
	if tr.Match[FieldTitleLink] != "receiver=slack-critical-kind" || tr.Match[FieldTitle] != "^custom$" {
		t.Errorf("match = %v, want the Binding's titleLink and title", tr.Match)
	}
	if tr.Exclude[FieldTitle] != "^Prometheus heartbeat$" {
		t.Errorf("exclude = %v", tr.Exclude)
	}
	if got := tr.Extract["key"]; got.Field != FieldFooter || got.Regex != `courier-key: (?P<key>\S+)` {
		t.Errorf("extract key = %+v, want the Binding's", got)
	}
	if tr.Thread.Cooldown.Duration != time.Hour || tr.Thread.CloseWhen["state"] != "ENDED" {
		t.Errorf("thread = %+v, want the Binding's cooldown and closeWhen", tr.Thread)
	}
	if tr.PromptTemplate != "Look at {{ .Key }}." {
		t.Errorf("promptTemplate = %q, want the Binding's", tr.PromptTemplate)
	}

	// What it left out comes from the preset.
	if tr.Match[FieldFallback] != preset.Match[FieldFallback] {
		t.Errorf("match fallback = %q, want the preset's", tr.Match[FieldFallback])
	}
	if tr.Extract["state"] != preset.Extract["state"] || tr.Extract["alertname"] != preset.Extract["alertname"] {
		t.Errorf("extract = %v, want the preset's state and alertname", tr.Extract)
	}
	if tr.Thread.By != ThreadByKey {
		t.Errorf("thread.by = %q, want the preset's key", tr.Thread.By)
	}
}

// TestPresetCloseWhenIsReplacedWhole pins that a Binding's closeWhen replaces the preset's,
// entry and all: it does not merge by key like match and extract do.
func TestPresetCloseWhenIsReplacedWhole(t *testing.T) {
	tr := Trigger{
		Type:   TriggerBotMessage,
		From:   &BotSource{BotID: "B0EXAMPLE01"},
		Preset: PresetAlertmanager,
		Extract: map[string]Extract{
			"severity": {Field: FieldText, Regex: `(?P<severity>critical|info)`},
		},
		Thread: TriggerThread{CloseWhen: map[string]string{"severity": "info"}},
	}
	tr.applyPreset()
	if want := map[string]string{"severity": "info"}; !maps.Equal(tr.Thread.CloseWhen, want) {
		t.Errorf("closeWhen = %v, want %v: the preset's state entry must not be merged in", tr.Thread.CloseWhen, want)
	}
}

func TestPresetApplicationIsIdempotentAndFresh(t *testing.T) {
	once := alertmanagerTrigger()
	twice := alertmanagerTrigger()
	twice.applyPreset()
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("applying the preset twice changed the trigger:\n%+v\n%+v", once, twice)
	}

	// Mutating one trigger must not leak into the next one's preset.
	once.Match[FieldTitle] = "changed"
	once.Thread.CloseWhen["state"] = "changed"
	if next := alertmanagerTrigger(); next.Match[FieldTitle] == "changed" || next.Thread.CloseWhen["state"] == "changed" {
		t.Errorf("the preset shares maps between triggers: %+v", next)
	}
}

func TestPresetOnlyAppliesToBotMessage(t *testing.T) {
	for _, tr := range []Trigger{
		{Type: TriggerMention, Preset: PresetAlertmanager},
		{Type: TriggerBotMessage, Preset: "grafana"},
		{Type: TriggerBotMessage},
	} {
		before := tr
		tr.applyPreset()
		if !reflect.DeepEqual(before, tr) {
			t.Errorf("applyPreset changed %+v to %+v", before, tr)
		}
	}
}

func TestDefaultTriggers(t *testing.T) {
	// Existing Bindings load unchanged: the mention trigger is the default.
	b := load[Binding](t, "binding-minimal.yaml")
	if want := []Trigger{{Type: TriggerMention}}; !reflect.DeepEqual(b.Spec.Chat.Triggers, want) {
		t.Errorf("triggers of a Binding without any = %+v, want %+v", b.Spec.Chat.Triggers, want)
	}

	// Explicit triggers are kept as the Binding wrote them, in order.
	b = load[Binding](t, "binding.yaml")
	ts := b.Spec.Chat.Triggers
	if len(ts) != 2 || ts[0].Type != TriggerMention || ts[1].Type != TriggerBotMessage ||
		ts[1].From.BotID != "B0123456789" || ts[1].Thread.Cooldown.Duration != 2*time.Hour ||
		ts[1].Exclude[FieldTitle] != "^Prometheus heartbeat$" {
		t.Errorf("triggers of binding.yaml = %+v", ts)
	}
}

// TestTriggerKeyIsType pins why the discriminator is not called "on": the YAML decoder reads
// a bare on as true, so the key would never arrive.
func TestTriggerKeyIsType(t *testing.T) {
	var ts []Trigger
	if err := yaml.UnmarshalStrict([]byte("- type: mention"), &ts); err != nil || len(ts) != 1 || ts[0].Type != TriggerMention {
		t.Fatalf("decoding type: mention = %+v, %v", ts, err)
	}
	if err := yaml.UnmarshalStrict([]byte("- on: mention"), &ts); err == nil {
		t.Fatal("decoding on: mention succeeded; the field name was chosen because it cannot")
	}
}

func TestParsePromptTemplate(t *testing.T) {
	if _, err := ParsePromptTemplate("t", "{{ .Fields.alertname }} in {{ .Channel }}"); err != nil {
		t.Errorf("a well-formed template: %v", err)
	}
	for _, text := range []string{"{{ .Fields.alertname ", "{{ nosuchfunc . }}", "{{ if }}"} {
		if _, err := ParsePromptTemplate("t", text); err == nil {
			t.Errorf("ParsePromptTemplate(%q) succeeded, want a parse error", text)
		}
	}
}

// bot returns the botMessage trigger of binding.yaml, after defaulting, which is [1].
func bot(o *Binding) *Trigger { return &o.Spec.Chat.Triggers[1] }

func TestTriggersValidate(t *testing.T) {
	const at = "spec.chat.triggers[1]"
	fieldsList := "text, title, titleLink, fallback, footer, attachmentText"
	hr := func(h time.Duration) metav1.Duration { return metav1.Duration{Duration: h} }
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"the fixture is valid", func(*Binding) {}, ""},
		{"no triggers at all", func(o *Binding) { o.Spec.Chat.Triggers = nil }, ""},
		{"unknown kind", func(o *Binding) { bot(o).Type = "message" },
			at + `.type: unsupported value "message", must be one of mention, botMessage`},
		{"mention with a preset", func(o *Binding) { o.Spec.Chat.Triggers[0].Preset = PresetAlertmanager },
			"spec.chat.triggers[0].preset: not allowed with type: mention"},
		{"mention with a source", func(o *Binding) { o.Spec.Chat.Triggers[0].From = &BotSource{BotID: "B0123456789"} },
			"spec.chat.triggers[0].from: not allowed with type: mention"},
		{"mention with a prompt", func(o *Binding) { o.Spec.Chat.Triggers[0].PromptTemplate = "x" },
			"spec.chat.triggers[0].promptTemplate: not allowed with type: mention"},
		{"mention with a thread rule", func(o *Binding) { o.Spec.Chat.Triggers[0].Thread.By = ThreadByKey },
			"spec.chat.triggers[0].thread: not allowed with type: mention"},
		{"mention with a maxIdle", func(o *Binding) { o.Spec.Chat.Triggers[0].Thread.MaxIdle = hr(48 * time.Hour) },
			"spec.chat.triggers[0].thread: not allowed with type: mention"},
		{"no source", func(o *Binding) { bot(o).From = nil }, at + ".from.botId: required"},
		{"empty bot ID", func(o *Binding) { bot(o).From.BotID = "" }, at + ".from.botId: required"},
		{"bot name instead of ID", func(o *Binding) { bot(o).From.BotID = "alertmanager" },
			at + `.from.botId: invalid bot ID "alertmanager": must be a Slack bot ID such as B0123456789, not a name`},
		{"user ID instead of bot ID", func(o *Binding) { bot(o).From.BotID = "U0123456789" },
			at + `.from.botId: invalid bot ID "U0123456789": must be a Slack bot ID such as B0123456789, not a name`},
		{"unknown preset", func(o *Binding) { bot(o).Preset = "grafana" },
			at + `.preset: unsupported value "grafana", must be one of alertmanager`},
		{"no preset and everything set by hand is fine", func(o *Binding) { bot(o).Preset = "" }, ""},

		// Regexes: they compile, within the cap, on known fields.
		{"match regex that does not compile", func(o *Binding) { bot(o).Match[FieldTitle] = "(" },
			at + ".match.title: invalid regular expression: error parsing regexp: missing closing ): `(`"},
		{"match regex with a lookahead, which RE2 lacks", func(o *Binding) { bot(o).Match[FieldTitle] = "^(?!x)" },
			at + ".match.title: invalid regular expression: error parsing regexp: invalid or unsupported Perl syntax: `(?!`"},
		{"exclude regex that does not compile", func(o *Binding) { bot(o).Exclude[FieldTitle] = "[" },
			at + ".exclude.title: invalid regular expression: error parsing regexp: missing closing ]: `[`"},
		{"empty match regex", func(o *Binding) { bot(o).Match[FieldTitle] = "" }, at + ".match.title: required"},
		{"match regex over the length cap", func(o *Binding) { bot(o).Match[FieldTitle] = strings.Repeat("a", 513) },
			at + ".match.title: regular expression is 513 bytes, longer than the 512 byte limit"},
		{"match regex at the length cap", func(o *Binding) { bot(o).Match[FieldTitle] = strings.Repeat("a", 512) }, ""},
		{"match on an unknown field", func(o *Binding) { bot(o).Match["subject"] = "x" },
			at + `.match.subject: unsupported message field "subject", must be one of ` + fieldsList},
		{"exclude on an unknown field", func(o *Binding) { bot(o).Exclude["subject"] = "x" },
			at + `.exclude.subject: unsupported message field "subject", must be one of ` + fieldsList},

		// Extracts: a known field, and a regex with the named group.
		{"extract regex that does not compile", func(o *Binding) {
			bot(o).Extract["alertname"] = Extract{Field: FieldTitle, Regex: "(?P<alertname>"}
		}, at + ".extract.alertname.regex: invalid regular expression: error parsing regexp: missing closing ): `(?P<alertname>`"},
		{"extract regex without its named group", func(o *Binding) {
			bot(o).Extract["key"] = Extract{Field: FieldFallback, Regex: `^\[[A-Z]+(?::\d+)?\] (?P<alert>[^(|]+)`}
		}, at + ".extract.key.regex: must have a named group (?P<key>...) that supplies the value"},
		{"extract regex with only an unnamed group", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Field: FieldTitle, Regex: `^(FIRING|RESOLVED)`}
		}, at + ".extract.state.regex: must have a named group (?P<state>...) that supplies the value"},
		{"extract regex over the length cap", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Field: FieldTitle, Regex: "(?P<state>a)" + strings.Repeat("a", 512)}
		}, at + ".extract.state.regex: regular expression is 524 bytes, longer than the 512 byte limit"},
		{"extract from an unknown field", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Field: "subject", Regex: `(?P<state>x)`}
		}, at + `.extract.state.field: unsupported message field "subject", must be one of ` + fieldsList},
		{"extract with no field", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Regex: `(?P<state>x)`}
		}, at + `.extract.state.field: unsupported message field "", must be one of ` + fieldsList},
		{"extract with no regex", func(o *Binding) { bot(o).Extract["state"] = Extract{Field: FieldTitle} },
			at + ".extract.state.regex: required"},
		{"extract name a template cannot read", func(o *Binding) {
			bot(o).Extract["Alert-Name"] = Extract{Field: FieldTitle, Regex: `(?P<Alert_Name>x)`}
		}, at + `.extract.Alert-Name: invalid extract name "Alert-Name": must start with a lower-case letter and use only letters, digits and '_', so a template can read .Fields.Alert-Name`},
		{"an extra extract is fine", func(o *Binding) {
			bot(o).Extract["env"] = Extract{Field: FieldText, Regex: `env=(?P<env>\w+)`}
		}, ""},

		// Thread rules.
		{"unknown thread.by", func(o *Binding) { bot(o).Thread.By = "message" },
			at + `.thread.by: unsupported value "message", must be one of key`},
		{"thread.by key without a key extract", func(o *Binding) {
			delete(bot(o).Extract, "key")
		}, at + `.thread.by: key requires an extract named "key"`},
		{"cooldown missing", func(o *Binding) { bot(o).Thread.Cooldown = metav1.Duration{} },
			at + ".thread.cooldown: required with thread.by"},
		{"cooldown negative", func(o *Binding) { bot(o).Thread.Cooldown = hr(-time.Hour) },
			at + ".thread.cooldown: -1h0m0s is out of range, must be between 1m0s and 168h0m0s"},
		{"cooldown under the floor", func(o *Binding) { bot(o).Thread.Cooldown = hr(59 * time.Second) },
			at + ".thread.cooldown: 59s is out of range, must be between 1m0s and 168h0m0s"},
		{"cooldown at the floor", func(o *Binding) { bot(o).Thread.Cooldown = hr(time.Minute) }, ""},
		{"cooldown at the ceiling", func(o *Binding) { bot(o).Thread.Cooldown = hr(168 * time.Hour) }, ""},
		{"cooldown over the ceiling", func(o *Binding) { bot(o).Thread.Cooldown = hr(169 * time.Hour) },
			at + ".thread.cooldown: 169h0m0s is out of range, must be between 1m0s and 168h0m0s"},
		{"closeWhen names a field that is not extracted", func(o *Binding) {
			bot(o).Thread.CloseWhen = map[string]string{"status": "RESOLVED"}
		}, at + ".thread.closeWhen.status: not an extracted field"},
		{"closeWhen with an empty value", func(o *Binding) { bot(o).Thread.CloseWhen["state"] = "" },
			at + ".thread.closeWhen.state: required"},
		{"closeWhen on an extra extracted field", func(o *Binding) {
			bot(o).Extract["env"] = Extract{Field: FieldText, Regex: `env=(?P<env>\w+)`}
			bot(o).Thread.CloseWhen = map[string]string{"env": "dev"}
		}, ""},
		{"no thread at all", func(o *Binding) { bot(o).Thread = TriggerThread{} }, ""},
		{"cooldown without thread.by", func(o *Binding) {
			bot(o).Thread = TriggerThread{Cooldown: hr(time.Hour)}
		}, at + ".thread.cooldown: requires thread.by"},
		{"closeWhen without thread.by", func(o *Binding) {
			bot(o).Thread = TriggerThread{CloseWhen: map[string]string{"state": "RESOLVED"}}
		}, at + ".thread.closeWhen: requires thread.by"},
		{"maxIdle without thread.by", func(o *Binding) {
			bot(o).Thread = TriggerThread{MaxIdle: hr(48 * time.Hour)}
		}, at + ".thread.maxIdle: requires thread.by"},
		{"maxIdle at twice the cooldown", func(o *Binding) { bot(o).Thread.MaxIdle = hr(4 * time.Hour) }, ""}, // the test Binding's cooldown is 2h
		{"maxIdle under twice the cooldown", func(o *Binding) { bot(o).Thread.MaxIdle = hr(4*time.Hour - time.Second) },
			at + ".thread.maxIdle: 3h59m59s is out of range, must be between twice the cooldown (4h0m0s) and 720h0m0s"},
		{"maxIdle at the ceiling", func(o *Binding) { bot(o).Thread.MaxIdle = hr(720 * time.Hour) }, ""},
		{"maxIdle over the ceiling", func(o *Binding) { bot(o).Thread.MaxIdle = hr(721 * time.Hour) },
			at + ".thread.maxIdle: 721h0m0s is out of range, must be between twice the cooldown (4h0m0s) and 720h0m0s"},
		{"maxIdle negative", func(o *Binding) { bot(o).Thread.MaxIdle = hr(-time.Hour) },
			at + ".thread.maxIdle: -1h0m0s is out of range, must be between twice the cooldown (4h0m0s) and 720h0m0s"},
		{"the default maxIdle with the longest cooldown", func(o *Binding) { bot(o).Thread.Cooldown = hr(168 * time.Hour) }, ""},

		// Prompt template.
		{"no prompt template", func(o *Binding) { bot(o).PromptTemplate = "" }, at + ".promptTemplate: required"},
		{"template that does not parse", func(o *Binding) { bot(o).PromptTemplate = "{{ .Fields.alertname " },
			at + `.promptTemplate: invalid template: template: promptTemplate:1: unclosed action`},
		{"template calling an unknown function", func(o *Binding) { bot(o).PromptTemplate = "{{ shout .State }}" },
			at + `.promptTemplate: invalid template: template: promptTemplate:1: function "shout" not defined`},

		// A walk of the parse tree checks every reference, in branches the load-time run does not
		// take, and then the template runs once against a view with a value for each extract.
		{"typo in a field name", func(o *Binding) { bot(o).PromptTemplate = "Alert {{ .Fields.alertnme }}" },
			at + `.promptTemplate: invalid template: promptTemplate:1:16: .Fields.alertnme is not an extract of this trigger (extract has alertname, key, state)`},
		{"typo in a top-level field", func(o *Binding) { bot(o).PromptTemplate = "State {{ .Stat }}" },
			at + `.promptTemplate: invalid template: promptTemplate:1:9: no field .Stat in the prompt view (it has .Channel, .State, .Key, .Link, .Source and .Fields)`},
		{"field of an extract this trigger lacks", func(o *Binding) {
			bot(o).PromptTemplate = "Env {{ .Fields.env }}"
		}, at + `.promptTemplate: invalid template: promptTemplate:1:14: .Fields.env is not an extract of this trigger (extract has alertname, key, state)`},
		{"typo in a field name in a branch the placeholder view does not take", func(o *Binding) {
			bot(o).PromptTemplate = `{{ if eq .State "FIRING" }}{{ .Fields.alertnme }}{{ end }}`
		}, at + `.promptTemplate: invalid template: promptTemplate:1:37: .Fields.alertnme is not an extract of this trigger (extract has alertname, key, state)`},
		{"typo in an index with a constant name, in a branch", func(o *Binding) {
			bot(o).PromptTemplate = `{{ if eq .State "FIRING" }}{{ index .Fields "alertnme" }}{{ else }}x{{ end }}`
		}, at + `.promptTemplate: invalid template: promptTemplate:1:30: .Fields.alertnme is not an extract of this trigger (extract has alertname, key, state)`},
		{"typo in a top-level field in the else branch", func(o *Binding) {
			bot(o).PromptTemplate = `{{ if eq .State "FIRING" }}x{{ else }}{{ .Stat }}{{ end }}`
		}, at + `.promptTemplate: invalid template: promptTemplate:1:41: no field .Stat in the prompt view (it has .Channel, .State, .Key, .Link, .Source and .Fields)`},
		{"the whole of .Fields", func(o *Binding) { bot(o).PromptTemplate = `{{ range .Fields }}{{ . }}{{ end }}` },
			at + `.promptTemplate: invalid template: promptTemplate:1:9: .Fields as a whole cannot be read: name an entry, such as .Fields.alertname`},
		{"a slice of the key that a long key fits", func(o *Binding) {
			bot(o).PromptTemplate = `{{ slice .Key 0 10 }} {{ slice .Fields.alertname 0 10 }}`
		}, ""},
		{"a template function used wrongly still fails the run", func(o *Binding) {
			bot(o).PromptTemplate = `{{ slice .Key 0 100 }}`
		}, at + `.promptTemplate: invalid template: template: promptTemplate:1:3: executing "promptTemplate" at <slice .Key 0 100>: error calling slice: index out of range: 100`},
		{"template reading every view field and every extract", func(o *Binding) {
			bot(o).PromptTemplate = "{{ .Channel }} {{ .State }} {{ .Key }} {{ .Link }} {{ .Source }} {{ .Fields.state }} {{ .Fields.key }} {{ .Fields.alertname }}"
		}, ""},

		// Duplicates.
		{"the same bot, match and exclude but a different extract is not a duplicate", func(o *Binding) {
			other := *bot(o)
			other.Extract = maps.Clone(other.Extract)
			other.Extract["alertname"] = Extract{Field: FieldTitle, Regex: `^FIRING \(\d+\) - (?P<alertname>.+)$`}
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, other)
		}, ""},
		{"two mention triggers", func(o *Binding) {
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, Trigger{Type: TriggerMention})
		}, "spec.chat.triggers[2]: duplicate of spec.chat.triggers[0]"},
		{"the same bot, match and exclude twice", func(o *Binding) {
			dup := *bot(o)
			dup.PromptTemplate = "another prompt"
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, dup)
		}, "spec.chat.triggers[2]: duplicate of spec.chat.triggers[1]"},
		{"the same bot with a different match is not a duplicate", func(o *Binding) {
			other := *bot(o)
			other.Match = maps.Clone(other.Match)
			other.Match[FieldTitleLink] = "receiver=slack-warning"
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, other)
		}, ""},
		{"another bot with the same match is not a duplicate", func(o *Binding) {
			other := *bot(o)
			other.From = &BotSource{BotID: "B0000000001"}
			o.Spec.Chat.Triggers = append(o.Spec.Chat.Triggers, other)
		}, ""},
	})
}

func TestTriggersValidateReportsEveryProblem(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	bot(b).From.BotID = "nope"
	bot(b).Match[FieldTitle] = "("
	bot(b).Thread.Cooldown = metav1.Duration{}
	want := []string{
		`spec.chat.triggers[1].from.botId: invalid bot ID "nope": must be a Slack bot ID such as B0123456789, not a name`,
		"spec.chat.triggers[1].match.title: invalid regular expression: error parsing regexp: missing closing ): `(`",
		"spec.chat.triggers[1].thread.cooldown: required with thread.by",
	}
	if got := problemsOf(b.Validate()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate() problems = %q, want %q", got, want)
	}
}

// An open thread is abandoned after maxIdle, which is 24h unless the trigger says, and never less than twice the
// cooldown, so that the default of a long cooldown is still one that validation accepts.
func TestEffectiveMaxIdle(t *testing.T) {
	hr := func(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }
	for _, tt := range []struct {
		name string
		th   TriggerThread
		want time.Duration
	}{
		{"the preset's cooldown", TriggerThread{Cooldown: hr(4 * time.Hour)}, 24 * time.Hour},
		{"a cooldown of exactly half the default", TriggerThread{Cooldown: hr(12 * time.Hour)}, 24 * time.Hour},
		{"a cooldown over half the default", TriggerThread{Cooldown: hr(168 * time.Hour)}, 336 * time.Hour},
		{"set", TriggerThread{Cooldown: hr(4 * time.Hour), MaxIdle: hr(10 * time.Hour)}, 10 * time.Hour},
	} {
		if got := tt.th.EffectiveMaxIdle(); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
