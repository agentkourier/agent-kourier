package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

const (
	backendDoc = `apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata: { name: kagent-pilot, namespace: agent-kourier-system }
spec: { dialect: kagent-v1, url: "http://kagent.example:8083" }
`
	connectionDoc = `apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-main, namespace: agent-kourier-system }
spec: { platform: slack, credentialsSecretRef: { name: slack-main } }
`
	bindingDoc = `apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: sre-alerts, namespace: sre }
spec:
  agent: { backendRef: { name: kagent-pilot }, name: k8s-agent, namespace: kagent }
  identity: { userId: agent-kourier-sre-alerts }
  chat: { connectionRef: { name: slack-main }, channel: C0123456789 }
`
)

// write creates the named files in a fresh directory and returns it.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadSpecExamples(t *testing.T) {
	set, err := Load(context.Background(), "../../api/v1alpha1/testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.AgentBackends) != 1 || len(set.ChatConnections) != 1 || len(set.Bindings) != 2 {
		t.Fatalf("got %d backends, %d connections, %d bindings; want 1, 1, 2",
			len(set.AgentBackends), len(set.ChatConnections), len(set.Bindings))
	}
	// Files load in lexical order: binding-minimal.yaml before binding.yaml.
	minimal := set.Bindings[0]
	if minimal.Name != "chat-only" || minimal.Spec.Chat.Output != v1alpha1.OutputLive ||
		minimal.Spec.Interactions.Timeout.Duration != 30*time.Minute || minimal.Spec.Interactions.ToolApprovals || len(minimal.Spec.Interactions.ApproverGroups) != 0 {
		t.Errorf("defaults not applied to %s: %+v", minimal.Name, minimal.Spec)
	}
}

func TestLoadToolApprovals(t *testing.T) {
	valid := bindingDoc + `  interactions:
    toolApprovals: true
    approverGroups: [S0123456789, S9876543210]
`
	set, err := Load(context.Background(), write(t, map[string]string{"b.yaml": valid}))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Bindings[0].Spec.Interactions; !got.ToolApprovals || !slices.Equal(got.ApproverGroups, []string{"S0123456789", "S9876543210"}) {
		t.Errorf("interactions = %+v; want enabled approvals and the configured groups", got)
	}

	withoutGroups := bindingDoc + "  interactions: { toolApprovals: true }\n"
	if set, err := Load(context.Background(), write(t, map[string]string{"b.yaml": withoutGroups})); err != nil || set == nil {
		t.Errorf("tool approvals without groups: Load = %+v, %v; want success", set, err)
	}

	withoutFlag := bindingDoc + "  interactions: { approverGroups: [S0123456789] }\n"
	if _, err := Load(context.Background(), write(t, map[string]string{"b.yaml": withoutFlag})); err == nil || !strings.Contains(err.Error(), "requires spec.interactions.toolApprovals: true") {
		t.Errorf("approver groups without tool approvals: Load error = %v; want the policy error", err)
	}
	emptyWithoutFlag := bindingDoc + "  interactions: { approverGroups: [] }\n"
	set, err = Load(context.Background(), write(t, map[string]string{"b.yaml": emptyWithoutFlag}))
	if err != nil {
		t.Fatalf("empty approver groups without tool approvals: Load error = %v; want success", err)
	}
	if len(set.Bindings) != 1 {
		t.Fatalf("loaded %d bindings; want one", len(set.Bindings))
	}
	if got := set.Bindings[0].Spec.Interactions; got.ToolApprovals || len(got.ApproverGroups) != 0 {
		t.Errorf("interactions = %+v; want approvals disabled and no approver groups", got)
	}
}

// withTriggers returns bindingDoc with the given triggers (YAML list items at six spaces) under
// spec.chat.
func withTriggers(triggers string) string {
	return strings.Replace(bindingDoc, "  chat: { connectionRef: { name: slack-main }, channel: C0123456789 }\n",
		"  chat:\n    connectionRef: { name: slack-main }\n    channel: C0123456789\n    triggers:\n"+triggers, 1)
}

// alertmanagerTriggerYAML is the botMessage trigger for Alertmanager's Slack receiver, written in full, as a list item
// at six spaces. %s stands for the extra match entries (a line each, at ten spaces) and %s for the limits block; the
// limits are what a botMessage trigger must set.
const alertmanagerTriggerYAML = `      - type: botMessage
        from: { botId: B0123456789 }
        match:
%s          title: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - '
          fallback: '^\[(?:FIRING(?::\d+)?|RESOLVED)\] '
        exclude: { title: '^Prometheus heartbeat$' }
        extract:
          state: { field: title, regex: '^(?:\[[^\]]{1,32}\] )?(?P<state>FIRING|RESOLVED)' }
          key: { field: fallback, regex: '^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])' }
          alertname: { field: title, regex: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - (?P<alertname>.+)$' }
        thread: { by: key, cooldown: 4h, closeWhen: { state: RESOLVED } }
%s        promptTemplate: "An alert is {{ .State }} in {{ .Channel }}: {{ .Fields.alertname }}."
`

// alertmanagerLimitsYAML is the limits block of the documented example.
const alertmanagerLimitsYAML = `        limits:
          rateLimit: { maxRuns: 5, per: 10m, maxConcurrent: 2 }
          maxRunsPerDay: 40
`

// alertmanagerTrigger fills alertmanagerTriggerYAML in: extraMatch is a line of the match block (or empty) and limits
// the limits block.
func alertmanagerTrigger(extraMatch, limits string) string {
	return fmt.Sprintf(alertmanagerTriggerYAML, extraMatch, limits)
}

// TestLoadAlertmanagerTriggerWithTrialNarrowing loads the owner's trial Binding: the Alertmanager trigger, written in
// full, for the channel's webhook bot, narrowed to the kind receiver. A Binding without triggers loads as before, with
// the mention trigger. Receiver regexes end in $ because receiver=slack-critical is a prefix of
// receiver=slack-critical-kind, so unanchored it would match both.
func TestLoadAlertmanagerTriggerWithTrialNarrowing(t *testing.T) {
	trial := withTriggers("      - type: mention\n" + alertmanagerTrigger("          titleLink: 'receiver=slack-critical-kind$'\n", alertmanagerLimitsYAML))
	set, err := Load(context.Background(), write(t, map[string]string{
		"plain.yaml": strings.Replace(bindingDoc, "sre-alerts", "plain", 1), "trial.yaml": trial}))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Bindings) != 2 {
		t.Fatalf("loaded %d bindings, want 2", len(set.Bindings))
	}

	plain := set.Bindings[0].Spec.Chat.Triggers
	if len(plain) != 1 || plain[0].Type != v1alpha1.TriggerMention {
		t.Errorf("triggers of a Binding without any = %+v, want just the mention trigger", plain)
	}

	ts := set.Bindings[1].Spec.Chat.Triggers
	if len(ts) != 2 {
		t.Fatalf("loaded %d triggers, want 2: %+v", len(ts), ts)
	}
	tr := ts[1]
	if tr.From.BotID != "B0123456789" {
		t.Errorf("botId = %q", tr.From.BotID)
	}
	if got := tr.Match[v1alpha1.FieldTitleLink]; got != "receiver=slack-critical-kind$" {
		t.Errorf("match.titleLink = %q, want the Binding's receiver", got)
	}
	if got := tr.Exclude[v1alpha1.FieldTitle]; got != "^Prometheus heartbeat$" {
		t.Errorf("exclude.title = %q, want the heartbeat", got)
	}
	if len(tr.Extract) != 3 || tr.Thread.By != v1alpha1.ThreadByKey || tr.Thread.Cooldown.Duration != 4*time.Hour ||
		tr.Thread.CloseWhen["state"] != "RESOLVED" || tr.PromptTemplate == "" {
		t.Errorf("trigger not loaded as written: %+v", tr)
	}
	if l := tr.Limits; l.RateLimit == nil || l.RateLimit.MaxRuns != 5 || l.RateLimit.Per.Duration != 10*time.Minute ||
		l.RateLimit.MaxConcurrent != 2 || l.MaxRunsPerDay == nil || *l.MaxRunsPerDay != 40 {
		t.Errorf("limits = %+v, want the ones written", l)
	}
}

// TestLoadKeepsAnExplicitNoDailyCap checks, through the loader, that maxRunsPerDay: 0 is a written choice and loads: it
// means no cap.
func TestLoadKeepsAnExplicitNoDailyCap(t *testing.T) {
	limits := strings.Replace(alertmanagerLimitsYAML, "maxRunsPerDay: 40", "maxRunsPerDay: 0", 1)
	set, err := Load(context.Background(), write(t, map[string]string{"b.yaml": withTriggers(alertmanagerTrigger("", limits))}))
	if err != nil {
		t.Fatal(err)
	}
	if n := set.Bindings[0].Spec.Chat.Triggers[0].Limits.MaxRunsPerDay; n == nil || *n != 0 {
		t.Errorf("maxRunsPerDay = %v, want an explicit 0", n)
	}
}

func TestLoadRejectsBadTriggers(t *testing.T) {
	const who = "b.yaml#1 Binding sre/sre-alerts: "
	const hint = "see the example in how-to/investigate-alertmanager-alerts (https://agentkourier.dev/how-to/investigate-alertmanager-alerts/)"
	tests := []struct{ name, triggers, want string }{
		{
			name:     "bare on, which YAML reads as true",
			triggers: "      - on: mention\n",
			want:     who + "spec.chat.triggers[0].true: unknown field\n" + who + "spec.chat.triggers[0].type: unsupported value \"\", must be one of mention, botMessage",
		},
		{
			name:     "regex that does not compile",
			triggers: alertmanagerTrigger("          text: '('\n", alertmanagerLimitsYAML),
			want:     who + "spec.chat.triggers[0].match.text: invalid regular expression: error parsing regexp: missing closing ): `(`",
		},
		{
			name:     "a preset, which is not a field",
			triggers: "      - { type: botMessage, from: { botId: B0123456789 }, preset: alertmanager }\n",
			want: who + "spec.chat.triggers[0].preset: unknown field\n" +
				who + "spec.chat.triggers[0].limits.rateLimit: required: set maxRuns, per and maxConcurrent; " + hint + "\n" +
				who + "spec.chat.triggers[0].limits.maxRunsPerDay: required: set the cap for a day, or 0 for no cap; " + hint + "\n" +
				who + "spec.chat.triggers[0].promptTemplate: required",
		},
		{
			name:     "no limits",
			triggers: alertmanagerTrigger("", ""),
			want: who + "spec.chat.triggers[0].limits.rateLimit: required: set maxRuns, per and maxConcurrent; " + hint + "\n" +
				who + "spec.chat.triggers[0].limits.maxRunsPerDay: required: set the cap for a day, or 0 for no cap; " + hint,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(context.Background(), write(t, map[string]string{"b.yaml": withTriggers(tc.triggers)}))
			if err == nil {
				t.Fatal("Load succeeded, want a problem")
			}
			var got []string
			for _, e := range flatten(err) {
				got = append(got, e.Error())
			}
			if strings.Join(got, "\n") != tc.want {
				t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), tc.want)
			}
		})
	}
}

func TestLoadMultiDocument(t *testing.T) {
	content := "---\n# leading comment\n" + backendDoc + "---\n---\n# only a comment\n---\n" + connectionDoc + "---\n" + bindingDoc
	set, err := Load(context.Background(), write(t, map[string]string{"all.yml": content}))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.AgentBackends) != 1 || len(set.ChatConnections) != 1 || len(set.Bindings) != 1 {
		t.Errorf("got %+v, want one of each kind", set)
	}
}

func TestLoadEmptyAndMissingDirectory(t *testing.T) {
	set, err := Load(context.Background(), write(t, map[string]string{"README.md": "# hi", "data.json": "{", "sub.yaml.bak": "[", ".hidden.yaml": "["}))
	if err != nil || set == nil || len(set.Bindings)+len(set.AgentBackends)+len(set.ChatConnections) != 0 {
		t.Errorf("Load(no YAML files) = %+v, %v; want an empty Set and no error", set, err)
	}
	_, err = Load(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(missing) error = %v, want one wrapping fs.ErrNotExist", err)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // exact problems, in order
	}{
		{
			name:  "wrong apiVersion",
			files: map[string]string{"b.yaml": strings.Replace(bindingDoc, "agentkourier.dev/v1alpha1", "agentkourier.dev/v1", 1)},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: apiVersion "agentkourier.dev/v1" is not supported, must be agentkourier.dev/v1alpha1`},
		},
		{
			name:  "unknown kind",
			files: map[string]string{"b.yaml": backendDoc + "---\n" + strings.Replace(bindingDoc, "kind: Binding", "kind: Route", 1)},
			want:  []string{`b.yaml#2 Route sre/sre-alerts: unknown kind "Route", must be one of AgentBackend, ChatConnection, Binding`},
		},
		{
			name:  "unknown field",
			files: map[string]string{"b.yaml": strings.NewReplacer("userId: agent-kourier-sre-alerts", "nope: 1", ", channel: C0123456789", "").Replace(bindingDoc) + "  bogus: 1\n"},
			want: []string{
				`b.yaml#1 Binding sre/sre-alerts: spec.bogus: unknown field`,
				`b.yaml#1 Binding sre/sre-alerts: spec.identity.nope: unknown field`,
				`b.yaml#1 Binding sre/sre-alerts: spec.identity: set userId, tokenSecretRef, or both`,
				`b.yaml#1 Binding sre/sre-alerts: spec.chat.channel: required`,
			},
		},
		{
			name:  "type error",
			files: map[string]string{"b.yaml": bindingDoc + "  interactions: { askUser: x }\n"},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: json: cannot unmarshal string into Go struct field Interactions.spec.interactions.askUser of type bool`},
		},
		{
			// A Binding's alerts block (a per-Binding Alertmanager webhook) was removed in DA-1: alerts reach a Binding
			// through a botMessage trigger, so a file that still carries the block no longer loads.
			name: "removed alerts field",
			files: map[string]string{"b.yaml": bindingDoc + "  alerts:\n    type: alertmanager\n    tokenSecretRef: { name: agent-kourier-webhook-token }\n" +
				"    promptTemplate: investigate\n    maxRunsPerDay: 40\n"},
			want: []string{`b.yaml#1 Binding sre/sre-alerts: spec.alerts: unknown field`},
		},
		{
			// A Binding has no status: its conditions were never set, and return with the controller that sets them.
			name:  "removed status field",
			files: map[string]string{"b.yaml": bindingDoc + "status:\n  conditions: []\n"},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: status: unknown field`},
		},
		{
			name: "removed directMessages field",
			files: map[string]string{"c.yaml": strings.Replace(connectionDoc, "credentialsSecretRef: { name: slack-main } }",
				"credentialsSecretRef: { name: slack-main }, directMessages: { defaultBindingRef: { name: sre-alerts, namespace: sre } } }", 1)},
			want: []string{`c.yaml#1 ChatConnection agent-kourier-system/slack-main: spec.directMessages: unknown field`},
		},
		{
			name:  "removed mode field",
			files: map[string]string{"c.yaml": strings.Replace(connectionDoc, "platform: slack,", "platform: slack, mode: socket,", 1)},
			want:  []string{`c.yaml#1 ChatConnection agent-kourier-system/slack-main: spec.mode: unknown field`},
		},
		{
			name:  "malformed YAML",
			files: map[string]string{"b.yaml": bindingDoc + "---\nmetadata: { name: x\n"},
			want:  []string{`b.yaml#2: document starting at line 9: yaml: line 1: did not find expected ',' or '}'`},
		},
		{
			name:  "bad separator",
			files: map[string]string{"b.yaml": bindingDoc + "--- foo\n"},
			want:  []string{`b.yaml#1: document starting at line 1: invalid Yaml document separator: foo`},
		},
		{
			name:  "number skips blank and comment-only documents",
			files: map[string]string{"b.yaml": "---\n" + backendDoc + "---\n# only a comment\n---\n" + strings.Replace(bindingDoc, ", channel: C0123456789", "", 1)},
			want:  []string{`b.yaml#2 Binding sre/sre-alerts: spec.chat.channel: required`},
		},
		{
			name:  "duplicate within a file",
			files: map[string]string{"b.yaml": bindingDoc + "---\n" + bindingDoc},
			want:  []string{`b.yaml#2 Binding sre/sre-alerts: duplicate of b.yaml#1`},
		},
		{
			name:  "validate failure",
			files: map[string]string{"bindings.yaml": backendDoc + "---\n" + connectionDoc + "---\n" + strings.Replace(bindingDoc, ", channel: C0123456789", "", 1)},
			want:  []string{`bindings.yaml#3 Binding sre/sre-alerts: spec.chat.channel: required`},
		},
		{
			name:  "not a mapping",
			files: map[string]string{"b.yaml": "- a\n- b\n"},
			want:  []string{`b.yaml#1: document must be a mapping`},
		},
		{
			name:  "duplicate across files",
			files: map[string]string{"a.yaml": bindingDoc, "b.yaml": connectionDoc + "---\n" + bindingDoc},
			want:  []string{`b.yaml#2 Binding sre/sre-alerts: duplicate of a.yaml#1`},
		},
		{
			name: "problems from several files are all reported",
			files: map[string]string{
				"a.yaml": strings.Replace(bindingDoc, "C0123456789", "general", 1),
				"b.yaml": strings.Replace(backendDoc, "kagent-v1", "grpc", 1),
			},
			want: []string{
				`a.yaml#1 Binding sre/sre-alerts: spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789, not a name`,
				`b.yaml#1 AgentBackend agent-kourier-system/kagent-pilot: spec.dialect: unsupported value "grpc", must be one of a2a, kagent-v1`,
			},
		},
		{
			name:  "file over the size limit",
			files: map[string]string{"big.yaml": "# " + strings.Repeat("x", maxFileSize) + "\n", "ok.yaml": bindingDoc},
			want:  []string{"big.yaml: file is larger than 1048576 bytes"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := Load(context.Background(), write(t, tc.files))
			if err == nil {
				t.Fatalf("Load succeeded with %+v, want problems %q", set, tc.want)
			}
			if set != nil {
				t.Errorf("Load returned a Set alongside an error: %+v", set)
			}
			var got []string
			for _, e := range flatten(err) {
				got = append(got, e.Error())
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestLoadFollowsSymlinks(t *testing.T) {
	// Kubernetes ConfigMap volumes expose each file as a symlink to a file elsewhere.
	target := filepath.Join(write(t, map[string]string{"b.yaml": bindingDoc}), "b.yaml")
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	if set, err := Load(context.Background(), dir); err != nil || len(set.Bindings) != 1 {
		t.Errorf("Load = %+v, %v; want the symlinked Binding", set, err)
	}
}

func TestLoadRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), dir); err == nil || err.Error() != "d.yaml: not a regular file" {
		t.Errorf("Load error = %v, want \"d.yaml: not a regular file\"", err)
	}
}

func TestLoadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx, write(t, map[string]string{"b.yaml": bindingDoc})); !errors.Is(err, context.Canceled) {
		t.Errorf("Load error = %v, want context.Canceled", err)
	}
}

func TestDecodeStrict(t *testing.T) {
	type inner struct {
		Count int `json:"count"`
	}
	type doc struct {
		Name  string `json:"name"`
		Inner inner  `json:"inner"`
	}
	tests := []struct {
		name, in string
		want     []string // substrings of the error, one per problem; none means it decodes
	}{
		{"valid", "name: a\ninner: {count: 1}\n", nil},
		{"an unknown field names its path", "name: a\ninner: {more: 1}\n", []string{"inner.more: unknown field"}},
		{"every unknown field is reported", "x: 1\ninner: {more: 1}\n", []string{"x: unknown field", "inner.more: unknown field"}},
		{"a duplicate key", "name: a\nname: b\n", []string{"already set"}},
		{"a type error", "name: [1]\n", []string{"name"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d doc
			err := DecodeStrict([]byte(tt.in), &d)
			if len(tt.want) == 0 {
				if err != nil || d.Inner.Count != 1 {
					t.Fatalf("DecodeStrict = %v, %+v; want it decoded", err, d)
				}
				return
			}
			if err == nil {
				t.Fatalf("DecodeStrict passed, want %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}
