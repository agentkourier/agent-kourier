package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
		minimal.Spec.Interactions.Timeout.Duration != 30*time.Minute {
		t.Errorf("defaults not applied to %s: %+v", minimal.Name, minimal.Spec)
	}
}

// withTriggers returns bindingDoc with the given triggers (YAML list items at six spaces) under
// spec.chat.
func withTriggers(triggers string) string {
	return strings.Replace(bindingDoc, "  chat: { connectionRef: { name: slack-main }, channel: C0123456789 }\n",
		"  chat:\n    connectionRef: { name: slack-main }\n    channel: C0123456789\n    triggers:\n"+triggers, 1)
}

// TestLoadAlertmanagerPresetWithTrialOverrides loads the owner's trial Binding: the
// alertmanager preset for the channel's webhook bot, narrowed to the kind receiver, with the
// heartbeat excluded. The preset supplies the rest, and a Binding without triggers loads as
// before, with the mention trigger. Receiver regexes end in $ because
// receiver=slack-critical is a prefix of receiver=slack-critical-kind, so unanchored it would
// match both; the preset itself names no receiver.
func TestLoadAlertmanagerPresetWithTrialOverrides(t *testing.T) {
	trial := withTriggers(`      - type: mention
      - type: botMessage
        from: { botId: B0123456789 }
        preset: alertmanager
        match: { titleLink: 'receiver=slack-critical-kind$' }
        exclude: { title: '^Prometheus heartbeat$' }
`)
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
	for _, f := range []v1alpha1.MessageField{v1alpha1.FieldTitle, v1alpha1.FieldFallback} {
		if tr.Match[f] == "" {
			t.Errorf("match.%s is empty, want the preset's", f)
		}
	}
	if got := tr.Exclude[v1alpha1.FieldTitle]; got != "^Prometheus heartbeat$" {
		t.Errorf("exclude.title = %q, want the heartbeat", got)
	}
	if len(tr.Extract) != 3 || tr.Thread.By != v1alpha1.ThreadByKey || tr.Thread.Cooldown.Duration != 4*time.Hour ||
		tr.Thread.CloseWhen["state"] != "RESOLVED" || tr.PromptTemplate == "" {
		t.Errorf("preset not applied: %+v", tr)
	}
}

// TestLoadPresetYieldsToTheBinding checks, through the loader, that what a Binding sets beats
// the preset's value for the same field.
func TestLoadPresetYieldsToTheBinding(t *testing.T) {
	doc := withTriggers(`      - type: botMessage
        from: { botId: B0123456789 }
        preset: alertmanager
        match: { title: '^MyAlert' }
        thread: { cooldown: 30m }
        promptTemplate: "Look at {{ .Key }}."
`)
	set, err := Load(context.Background(), write(t, map[string]string{"b.yaml": doc}))
	if err != nil {
		t.Fatal(err)
	}
	tr := set.Bindings[0].Spec.Chat.Triggers[0]
	if tr.Match[v1alpha1.FieldTitle] != "^MyAlert" || tr.Thread.Cooldown.Duration != 30*time.Minute ||
		tr.PromptTemplate != "Look at {{ .Key }}." {
		t.Errorf("the preset overrode the Binding: match.title %q, cooldown %v, prompt %q",
			tr.Match[v1alpha1.FieldTitle], tr.Thread.Cooldown.Duration, tr.PromptTemplate)
	}
}

func TestLoadRejectsBadTriggers(t *testing.T) {
	tests := []struct{ name, triggers, want string }{
		{
			name:     "bare on, which YAML reads as true",
			triggers: "      - on: mention\n",
			want:     "b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].true: unknown field\nb.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].type: unsupported value \"\", must be one of mention, botMessage",
		},
		{
			name:     "regex that does not compile",
			triggers: "      - { type: botMessage, from: { botId: B0123456789 }, preset: alertmanager, match: { title: '(' } }\n",
			want:     "b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].match.title: invalid regular expression: error parsing regexp: missing closing ): `(`",
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
			// Fields of features that are not built are not in the API: they return with the feature.
			name:  "removed interactions.approverGroups field",
			files: map[string]string{"b.yaml": bindingDoc + "  interactions: { approverGroups: [sre-oncall] }\n"},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: spec.interactions.approverGroups: unknown field`},
		},
		{
			name:  "removed interactions.toolApprovals field",
			files: map[string]string{"b.yaml": bindingDoc + "  interactions: { toolApprovals: false }\n"},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: spec.interactions.toolApprovals: unknown field`},
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
