package recipes_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// TestRecipesApplyTogether loads the kourier/ files of every recipe into one config, as copying them all into one
// config directory would, resolves it, and has the broker pick its ChatConnection. Agent Kourier reads one
// directory and serves exactly one ChatConnection, so recipes can be applied side by side only if
//
//   - they ship the same ChatConnection, which is kept once;
//   - no other object (same kind, namespace and name) is in two of them; and
//   - no two Bindings are on one channel.
//
// A new recipe that ships a different ChatConnection, reuses a name or reuses a channel placeholder fails it.
func TestRecipesApplyTogether(t *testing.T) {
	recipes, err := recipeDirs(recipesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) == 0 {
		t.Skipf("no recipes under %s: nothing to apply together", recipesDir)
	}
	if err := checkTogether(t.Context(), recipes); err != nil {
		t.Error(err)
	}
}

// TestCheckTogether runs the checker on recipes built here: ones that coexist, and the clashes it must refuse.
func TestCheckTogether(t *testing.T) {
	type files map[string]string
	// connection is a recipe's ChatConnection document; note is a comment line, which changes the bytes and not the object.
	connection := func(name, secretName, note string) string {
		return fmt.Sprintf(`%sapiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: %[2]s, namespace: recipe }
spec: { platform: slack, credentialsSecretRef: { name: %[3]s } }
`, note, name, secretName)
	}
	backend := func(name string) string {
		return fmt.Sprintf(`apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata: { name: %s, namespace: recipe }
spec: { dialect: a2a, url: "http://agent.recipe:8080" }
`, name)
	}
	binding := func(name, connection, channel string) string {
		return fmt.Sprintf(`apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: %[1]s, namespace: recipe }
spec:
  agent: { backendRef: { name: %[1]s-agents }, name: helper, namespace: recipe }
  identity: { userId: agent-kourier-%[1]s }
  chat: { connectionRef: { name: %[2]s }, channel: %[3]s }
`, name, connection, channel)
	}
	// recipe is a recipe named name, with its own backend, a Binding on the channel, and the given ChatConnection document.
	recipe := func(name, channel, conn string) files {
		return files{"backend.yaml": backend(name + "-agents"), "binding.yaml": binding(name, "slack", channel), "connection.yaml": conn}
	}
	slack := connection("slack", "slack", "")

	tests := []struct {
		name    string
		recipes map[string]files
		want    []string // substrings of the error; none means the recipes must coexist
	}{
		{name: "one recipe", recipes: map[string]files{"a": recipe("a", "CAAAAAAAA", slack)}},
		{name: "the same ChatConnection, kept once", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": recipe("b", "CBBBBBBBB", connection("slack", "slack", "# another comment\n")), // equal as an object, not as bytes
		}},
		{name: "a ChatConnection of the same name with another spec", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": recipe("b", "CBBBBBBBB", connection("slack", "slack-ci", "")),
		}, want: []string{"b-connection.yaml#1 ChatConnection recipe/slack", "duplicate of a-connection.yaml#1", "only if identical"}},
		{name: "two different ChatConnections", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": func() files {
				f := recipe("b", "CBBBBBBBB", connection("slack-b", "slack-b", ""))
				f["binding.yaml"] = binding("b", "slack-b", "CBBBBBBBB")
				return f
			}(),
		}, want: []string{"exactly one ChatConnection", "recipe/slack", "recipe/slack-b"}},
		{name: "two recipes on one channel", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": recipe("b", "CAAAAAAAA", slack),
		}, want: []string{"b-binding.yaml#1 Binding recipe/b", "CAAAAAAAA on ChatConnection recipe/slack is already bound by Binding recipe/a"}},
		{name: "the same Binding in two recipes", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": func() files {
				f := recipe("b", "CBBBBBBBB", slack)
				f["binding.yaml"] = strings.Replace(f["binding.yaml"], "name: b,", "name: a,", 1)
				return f
			}(),
		}, want: []string{"b-binding.yaml#1 Binding recipe/a", "duplicate of a-binding.yaml#1"}},
		{name: "an identical AgentBackend is still a duplicate", recipes: map[string]files{
			"a": recipe("a", "CAAAAAAAA", slack),
			"b": func() files {
				f := recipe("b", "CBBBBBBBB", slack)
				f["backend.yaml"] = backend("a-agents")
				return f
			}(),
		}, want: []string{"b-backend.yaml#1 AgentBackend recipe/a-agents", "duplicate of a-backend.yaml#1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			var dirs []string
			for name, fs := range tt.recipes {
				dir := filepath.Join(root, name)
				dirs = append(dirs, dir)
				if err := os.MkdirAll(filepath.Join(dir, "kourier"), 0o750); err != nil {
					t.Fatal(err)
				}
				for file, content := range fs {
					if err := os.WriteFile(filepath.Join(dir, "kourier", file), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := checkTogether(t.Context(), dirs)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("checkTogether: %v; want the recipes to coexist", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkTogether passed; want an error containing %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// checkTogether puts the kourier/ files of the recipes at dirs into one temporary directory, each file as
// "<recipe>-<file>" so that no two share a name, then loads and resolves it as the broker would and has the broker
// choose its ChatConnection. The Secrets are taken as present, as in checkRecipe.
//
// Recipes ship the same ChatConnection so that they can run under one broker: a ChatConnection document that
// repeats an earlier recipe's, decoded to an equal object, is left out, and the first copy is kept. Any other
// repeated identity is left in for Load to refuse. A problem names the copied file, so "a-binding.yaml" is
// binding.yaml of recipe a; a file that lost a document counts the rest from 1.
func checkTogether(ctx context.Context, dirs []string) error {
	together, err := os.MkdirTemp("", "recipes-together-")
	if err != nil {
		return fmt.Errorf("make a directory for the recipes: %w", err)
	}
	defer func() { _ = os.RemoveAll(together) }()
	shared := map[string]v1alpha1.ChatConnection{} // "namespace/name" of the ChatConnections kept so far
	for _, dir := range slices.Sorted(slices.Values(dirs)) {
		kourier := filepath.Join(dir, "kourier")
		entries, err := os.ReadDir(kourier)
		if err != nil {
			return fmt.Errorf("read %s: %w", kourier, err)
		}
		for _, e := range entries {
			if ext := filepath.Ext(e.Name()); e.IsDir() || strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(kourier, e.Name()))
			if err != nil {
				return fmt.Errorf("read recipe file: %w", err)
			}
			if data, err = withoutRepeatedConnections(data, shared); err != nil {
				return fmt.Errorf("%s: %w", filepath.Join(kourier, e.Name()), err)
			}
			if data == nil {
				continue // nothing but repeated ChatConnections
			}
			if err := os.WriteFile(filepath.Join(together, filepath.Base(dir)+"-"+e.Name()), data, 0o600); err != nil {
				return fmt.Errorf("copy recipe file: %w", err)
			}
		}
	}
	set, err := config.Load(ctx, together)
	if err != nil {
		return fmt.Errorf("recipes applied together (a file is named <recipe>-<file>; a ChatConnection may be in several recipes only if identical): %w", err)
	}
	snap, err := config.Resolve(ctx, set, config.PresentSecrets(set))
	if err != nil {
		return fmt.Errorf("recipes applied together (a file is named <recipe>-<file>): %w", err)
	}
	// The broker serves one ChatConnection (internal/broker/chatrun.go, ChooseConnection), which Resolve does not check.
	if _, err := broker.ChooseConnection(ctx, snap, fakeSecrets{}); err != nil {
		return fmt.Errorf("recipes applied together: %w", err)
	}
	return nil
}

// withoutRepeatedConnections returns the YAML in data without the ChatConnection documents that equal one in kept,
// and adds the ones it keeps to kept. Documents of other kinds, and ChatConnections that differ from the one kept
// under their name, stay. Comments and formatting do not count: the decoded objects are compared. It returns nil
// when nothing is left. A document it cannot decode stays for Load to report.
func withoutRepeatedConnections(data []byte, kept map[string]v1alpha1.ChatConnection) ([]byte, error) {
	var docs [][]byte
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read documents: %w", err)
		}
		var m metav1.PartialObjectMetadata
		var c v1alpha1.ChatConnection
		if yaml.Unmarshal(doc, &m) != nil || m.Kind != "ChatConnection" || m.Name == "" || yaml.Unmarshal(doc, &c) != nil {
			docs = append(docs, doc)
			continue
		}
		key := c.Namespace + "/" + c.Name
		if first, ok := kept[key]; ok && reflect.DeepEqual(first, c) {
			continue
		} else if !ok {
			kept[key] = c
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

// fakeSecrets gives every key of every Secret a fake value, for the broker's choice of a connection.
type fakeSecrets struct{}

func (fakeSecrets) Value(_ context.Context, _, _, key string) (secret.Value, error) {
	return secret.New("fake-" + key), nil
}
