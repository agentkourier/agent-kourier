// Package crdcorpus is the set of objects that Agent Kourier's CRD schemas and its own Default and Validate are held
// to agree on (spec D50, decision 2). The invariant is that every object the schema refuses, the broker refuses too,
// so that the same YAML cannot behave differently on the file route and the CR route.
//
// The corpus has two halves, each read by two tests. The valid half is every resource that already exists in the
// repository (the API's fixtures, the examples, the kind tiers' configs, the recipes) and testdata/valid, which holds
// the edges of what the schema's markers allow. The invalid half, testdata/invalid, has an entry for each rule of
// api/v1alpha1/validate.go. The unit test in this package runs both halves through the file loader, which is the
// broker's own path, and `make crd-corpus` (the crdcorpus build tag) sends each entry to a real apiserver that has the
// CRDs, as a server-side dry-run create.
//
// An invalid entry is a YAML document, in a file of documents separated by "---", that begins with comment lines:
//
//	# rule: binding/spec.chat.channel: required     what the entry shows; unique
//	# want: spec.chat.channel: required             text the broker's refusal must contain
//	# schema: refuses                               or "accepts": what the apiserver's schema does with it
//	# field: spec.chat.channel                      optional: the field the schema's refusal must name; by default the
//	                                                text of want before its first ": "
//	# cluster: skip because ...                     optional: not sent to an apiserver, with the reason
//
// The invariant is about the fields of a resource's spec, which the markers copy. An apiserver validates an object's metadata
// as well (a label or annotation it does not like, an ownerReference), and may refuse what the broker, which reads only
// the name and namespace, accepts. That is the apiserver's rule and not a marker, so it is outside the invariant, and
// the corpus has no entry for it.
//
// "schema: accepts" is a rule only the broker enforces. It is how this package says which rules the CRD does not copy
// (yet), so the day a marker or CEL rule starts to refuse an entry, the test fails until the flag follows it.
package crdcorpus

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// Entry is one object of the corpus.
type Entry struct {
	// Name says where the entry is from: "path#n" for a valid one, its rule for an invalid one.
	Name string
	// Kind is the object's kind.
	Kind string
	// Namespace is the object's metadata.namespace, which may be empty or not a valid namespace.
	Namespace string
	// Object is the document as a generic map, the form an apiserver client sends.
	Object map[string]any

	// The fields below are set for an invalid entry only.

	// Want is text that the broker's refusal contains.
	Want string
	// SchemaRefuses says whether the CRD's schema refuses the object.
	SchemaRefuses bool
	// Field is the field that the schema's refusal must name, when it refuses: the entry's "# field:" line, or the text
	// of Want before its first ": ". It is empty if neither says.
	Field string
	// SkipCluster, if set, is why the entry is not sent to an apiserver.
	SkipCluster string
}

// YAML returns the entry as the YAML document the file loader would read.
func (e Entry) YAML() ([]byte, error) {
	b, err := yaml.Marshal(e.Object)
	if err != nil {
		return nil, fmt.Errorf("crdcorpus: marshal %s: %w", e.Name, err)
	}
	return b, nil
}

// Broker runs the entry through the broker's own loader (config.Load, as a file route broker starts) and returns the
// refusal, or nil if the broker accepts it. The loader decodes strictly, defaults, validates and checks the
// apiVersion and kind, so what it accepts is what a broker serves.
func Broker(ctx context.Context, e Entry) error {
	doc, err := e.YAML()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "crdcorpus-")
	if err != nil {
		return fmt.Errorf("crdcorpus: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "entry.yaml"), doc, 0o600); err != nil {
		return fmt.Errorf("crdcorpus: %w", err)
	}
	_, err = config.Load(ctx, dir)
	return err
}

// Root returns the repository's root, found from the working directory by its go.mod.
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("crdcorpus: no go.mod above the working directory")
		}
		dir = parent
	}
}

// InvalidEntries is how many entries the invalid half has. Both tests hold the corpus to it, so an entry cannot be
// dropped, or added, without this changing in the same commit.
const InvalidEntries = 152

// MinValidEntries is the fewest valid entries the valid half may have: the repository's resources change, so this is a
// floor and not a count. It is under what the roots hold today (see TestEveryValidEntryIsAcceptedByTheBroker), and
// over what a glob that stopped matching would leave.
const MinValidEntries = 90

// CheckSize reports a corpus that is smaller than it must be, which a test would otherwise pass on: a renamed
// directory or a deleted entry empties the half a test runs over, and the loop over it still succeeds.
func CheckSize(valid, invalid []Entry) error {
	if len(invalid) != InvalidEntries {
		return fmt.Errorf("crdcorpus: %d invalid entries, want %d (InvalidEntries): an entry was dropped or added without the count", len(invalid), InvalidEntries)
	}
	if len(valid) < MinValidEntries {
		return fmt.Errorf("crdcorpus: %d valid entries, want at least %d (MinValidEntries)", len(valid), MinValidEntries)
	}
	return nil
}

// Problems splits the refusal of Broker into its problems, one for each rule the object breaks.
func Problems(err error) []error {
	var j interface{ Unwrap() []error }
	if errors.As(err, &j) {
		return j.Unwrap()
	}
	if err == nil {
		return nil
	}
	return []error{err}
}

// templates are the two files, by path under the repository's root, that are not resources yet: the sandbox tier's script
// fills a placeholder in each (__SLACK_CHANNEL_ID__, __ALERT_CHANNEL_ID__) with the channel's ID. They are named, and not
// found by their text, so that a placeholder anywhere else is a channel the broker refuses, which fails the test, and a
// comment that mentions one cannot drop a real file from the corpus.
var templates = []string{
	"hack/kind/sandbox/agent-kourier/config/binding.yaml",
	"hack/kind/sandbox/agent-kourier/alert-trial/alert-binding.yaml",
}

// validRoots are the directories, under the repository's root, that hold the valid half: every v1alpha1 resource in
// them is an entry, but the templates. The recipe directories hold only what the recipes' own tests load.
var validRoots = []string{
	"api/v1alpha1/testdata",
	"examples",
	"hack/kind",
	"test/recipes/testdata/valid",
	"test/recipes/testdata/evals-pass",
	"test/crdcorpus/testdata/valid",
}

// Valid returns the valid half of the corpus, in a stable order. Each entry is a resource the broker accepts.
func Valid(root string) ([]Entry, error) {
	var entries []Entry
	for _, r := range validRoots {
		err := filepath.WalkDir(filepath.Join(root, r), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ext := filepath.Ext(path); d.IsDir() || (ext != ".yaml" && ext != ".yml") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path) //nolint:gosec // path comes from walking this repository's own corpus directories
			if err != nil {
				return err
			}
			if slices.Contains(templates, filepath.ToSlash(rel)) {
				return nil
			}
			docs, err := splitDocuments(data)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			for n, doc := range docs {
				obj, err := decode(doc)
				if err != nil {
					return fmt.Errorf("%s#%d: %w", rel, n+1, err)
				}
				if obj == nil || obj["apiVersion"] != v1alpha1.APIVersion {
					continue // a Secret, a ConfigMap or the like, in a file the tiers apply as one
				}
				entries = append(entries, newEntry(fmt.Sprintf("%s#%d", rel, n+1), obj))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("crdcorpus: %w", err)
		}
	}
	return entries, nil
}

// Invalid returns the invalid half of the corpus, from the files of testdata/invalid, in a stable order.
func Invalid(root string) ([]Entry, error) {
	dir := filepath.Join(root, "test/crdcorpus/testdata/invalid")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("crdcorpus: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("crdcorpus: no invalid corpus in %s", dir)
	}
	slices.Sort(files)
	var entries []Entry
	seen := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // f is a match of a glob in this repository's own corpus directory
		if err != nil {
			return nil, fmt.Errorf("crdcorpus: %w", err)
		}
		docs, err := splitDocuments(data)
		if err != nil {
			return nil, fmt.Errorf("crdcorpus: %s: %w", filepath.Base(f), err)
		}
		for n, doc := range docs {
			at := fmt.Sprintf("%s#%d", filepath.Base(f), n+1)
			e, err := parseInvalid(doc)
			if err != nil {
				return nil, fmt.Errorf("crdcorpus: %s: %w", at, err)
			}
			if first, dup := seen[e.Name]; dup {
				return nil, fmt.Errorf("crdcorpus: %s: rule %q is also at %s", at, e.Name, first)
			}
			seen[e.Name] = at
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// splitDocuments cuts a file into its YAML documents, dropping the ones that are empty or only comments.
func splitDocuments(data []byte) ([][]byte, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(stripComments(doc))) > 0 {
			docs = append(docs, doc)
		}
	}
}

// stripComments drops the lines that are only a comment.
func stripComments(doc []byte) []byte {
	var out []byte
	for line := range bytes.SplitSeq(doc, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			out = append(append(out, line...), '\n')
		}
	}
	return out
}

func decode(doc []byte) (map[string]any, error) {
	j, err := yaml.YAMLToJSONStrict(doc)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := yaml.Unmarshal(j, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func newEntry(name string, obj map[string]any) Entry {
	e := Entry{Name: name, Object: obj}
	e.Kind, _ = obj["kind"].(string)
	if meta, ok := obj["metadata"].(map[string]any); ok {
		e.Namespace, _ = meta["namespace"].(string)
	}
	return e
}

// parseInvalid reads one document of an invalid file: the comment header, then the object.
func parseInvalid(doc []byte) (Entry, error) {
	var rule, want, schema, skip, field string
	for line := range strings.SplitSeq(string(doc), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		for key, dst := range map[string]*string{"# rule: ": &rule, "# want: ": &want, "# schema: ": &schema, "# cluster: ": &skip, "# field: ": &field} {
			if v, ok := strings.CutPrefix(line, key); ok {
				*dst = v
			}
		}
	}
	obj, err := decode(doc)
	if err != nil {
		return Entry{}, err
	}
	if obj == nil {
		return Entry{}, errors.New("no object")
	}
	e := newEntry(rule, obj)
	e.Want = want
	e.Field = field
	if e.Field == "" {
		if path, _, ok := strings.Cut(want, ": "); ok && !strings.ContainsAny(path, " \"") {
			e.Field = path
		}
	}
	switch {
	case rule == "":
		return Entry{}, errors.New("no '# rule:' line")
	case want == "":
		return Entry{}, fmt.Errorf("%s: no '# want:' line", rule)
	case skip != "":
		if schema != "" {
			return Entry{}, fmt.Errorf("%s: a '# cluster:' entry is not sent to a schema, so it has no '# schema:' line", rule)
		}
		e.SkipCluster = skip
	case schema == "refuses":
		e.SchemaRefuses = true
	case schema != "accepts":
		return Entry{}, fmt.Errorf("%s: '# schema:' must be refuses or accepts, got %q", rule, schema)
	}
	return e, nil
}
