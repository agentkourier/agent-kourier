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
	backendDoc = `apiVersion: agentcourier.dev/v1alpha1
kind: AgentBackend
metadata: { name: kagent-pilot, namespace: courier-system }
spec: { dialect: kagent-v1, url: "http://kagent.example:8083" }
`
	connectionDoc = `apiVersion: agentcourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-main, namespace: courier-system }
spec: { platform: slack, credentialsSecretRef: { name: slack-main } }
`
	bindingDoc = `apiVersion: agentcourier.dev/v1alpha1
kind: Binding
metadata: { name: sre-alerts, namespace: sre }
spec:
  agent: { backendRef: { name: kagent-pilot }, name: k8s-agent, namespace: kagent }
  identity: { userId: courier-sre-alerts }
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
			files: map[string]string{"b.yaml": strings.Replace(bindingDoc, "agentcourier.dev/v1alpha1", "agentcourier.dev/v1", 1)},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: apiVersion "agentcourier.dev/v1" is not supported, must be agentcourier.dev/v1alpha1`},
		},
		{
			name:  "unknown kind",
			files: map[string]string{"b.yaml": backendDoc + "---\n" + strings.Replace(bindingDoc, "kind: Binding", "kind: Route", 1)},
			want:  []string{`b.yaml#2 Route sre/sre-alerts: unknown kind "Route", must be one of AgentBackend, ChatConnection, Binding`},
		},
		{
			name:  "unknown field",
			files: map[string]string{"b.yaml": strings.NewReplacer("userId: courier-sre-alerts", "nope: 1", ", channel: C0123456789", "").Replace(bindingDoc) + "  bogus: 1\n"},
			want: []string{
				`b.yaml#1 Binding sre/sre-alerts: spec.bogus: unknown field`,
				`b.yaml#1 Binding sre/sre-alerts: spec.identity.nope: unknown field`,
				`b.yaml#1 Binding sre/sre-alerts: spec.identity: set userId, tokenSecretRef, or both`,
				`b.yaml#1 Binding sre/sre-alerts: spec.chat.channel: required`,
			},
		},
		{
			name:  "type error",
			files: map[string]string{"b.yaml": bindingDoc + "  alerts: { maxRunsPerDay: x }\n"},
			want:  []string{`b.yaml#1 Binding sre/sre-alerts: json: cannot unmarshal string into Go struct field Alerts.spec.alerts.maxRunsPerDay of type int32`},
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
				`b.yaml#1 AgentBackend courier-system/kagent-pilot: spec.dialect: unsupported value "grpc", must be one of a2a, kagent-v1`,
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
