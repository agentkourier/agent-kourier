// Package config loads courier's resources (AgentBackend, ChatConnection, Binding) from YAML
// files (D30, spec section 7). It checks one object at a time; cross-object checks belong to
// the snapshot builder.
package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// maxFileSize is the largest config file Load reads, so a runaway file cannot exhaust memory.
const maxFileSize = 1 << 20

// Set holds every object found in a config directory, defaulted and validated, in file then
// document order. References between objects are not resolved.
type Set struct {
	AgentBackends   []v1alpha1.AgentBackend
	ChatConnections []v1alpha1.ChatConnection
	Bindings        []v1alpha1.Binding
}

// Load reads every *.yaml and *.yml file directly inside dir, in lexical order. Each file may
// hold several documents separated by "---". Documents that are empty or only comments are
// skipped and not counted. Each other document must carry the v1alpha1 apiVersion and a known
// kind, decode without unknown fields, and pass Default and Validate.
//
// Load reports every problem it finds, joined, each prefixed with "<file>#<n> <kind>
// <namespace>/<name>:" (n is the 1-based document number in the file) and returns a nil Set.
// A missing directory gives an error wrapping fs.ErrNotExist.
func Load(ctx context.Context, dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read config directory: %w", err)
	}
	l := &loader{set: &Set{}, seen: map[identity]string{}}
	for _, e := range entries {
		// Dot names include ConfigMap's ..data entries and editors' dangling lock symlinks.
		if ext := filepath.Ext(e.Name()); strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
		l.file(dir, e.Name())
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return l.set, nil
}

// identity is what makes an object unique: two documents may not share it.
type identity struct{ kind, namespace, name string }

// String renders what is known of the identity, as in "Binding sre/sre-alerts".
func (id identity) String() string {
	return strings.TrimSpace(id.kind + " " + path.Join(id.namespace, id.name))
}

type loader struct {
	set  *Set
	seen map[identity]string // first location of each object, as "file#n"
	errs []error
}

// fail records problems found at loc, the location and whatever is known of the object.
func (l *loader) fail(loc string, errs ...error) {
	for _, err := range errs {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", loc, err))
	}
}

func (l *loader) file(dir, name string) {
	data, err := readFile(filepath.Join(dir, name))
	if err != nil {
		l.fail(name, err)
		return
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	// The reader drops the separator that ends each document, so each starts one line
	// after the previous one's last.
	for n, line := 1, 1; ; {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			l.fail(fmt.Sprintf("%s#%d", name, n), atLine(line, err))
			return
		}
		if !blank(doc) {
			l.document(name, n, line, doc)
			n++
		}
		line += bytes.Count(doc, []byte("\n")) + 1
	}
}

// atLine tells the operator where in the file a YAML error's own line numbers start.
func atLine(line int, err error) error {
	return fmt.Errorf("document starting at line %d: %w", line, err)
}

// document decodes one non-blank document from file, where n is its number in the file and
// line the line it starts on.
func (l *loader) document(file string, n, line int, doc []byte) {
	at := fmt.Sprintf("%s#%d", file, n)
	j, err := yaml.YAMLToJSONStrict(doc) // also rejects duplicate keys
	if err != nil {
		l.fail(at, atLine(line, err))
		return
	}
	if j[0] != '{' {
		l.fail(at, errors.New("document must be a mapping"))
		return
	}
	// A type error here (say, metadata is a string) still fills in what it can; the typed
	// decode below reports it properly, so the error is dropped.
	var h metav1.PartialObjectMetadata
	_ = json.Unmarshal(j, &h)
	id := identity{h.Kind, h.Namespace, h.Name}
	loc := strings.TrimSpace(at + " " + id.String())
	if h.APIVersion != v1alpha1.APIVersion {
		l.fail(loc, fmt.Errorf("apiVersion %q is not supported, must be %s", h.APIVersion, v1alpha1.APIVersion))
		return
	}
	var errs []error
	switch h.Kind {
	case "AgentBackend":
		errs = decode(j, &l.set.AgentBackends)
	case "ChatConnection":
		errs = decode(j, &l.set.ChatConnections)
	case "Binding":
		errs = decode(j, &l.set.Bindings)
	default:
		l.fail(loc, fmt.Errorf("unknown kind %q, must be one of AgentBackend, ChatConnection, Binding", h.Kind))
		return
	}
	l.fail(loc, errs...)
	if id.name == "" {
		return // Validate reports the missing name
	}
	if first, dup := l.seen[id]; dup {
		l.fail(loc, fmt.Errorf("duplicate of %s", first))
	} else {
		l.seen[id] = at
	}
}

// decode strictly decodes the JSON in j into a T, defaults and validates it, and if all is
// well appends it to dst. Unknown fields do not stop decoding, so they are reported together
// with the Validate problems, each as "path: message". PT is *T.
func decode[T any, PT interface {
	*T
	Default()
	Validate() error
}](j []byte, dst *[]T) []error {
	obj := new(T)
	strict, err := kjson.UnmarshalStrict(j, obj, kjson.DisallowUnknownFields)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, e := range strict {
		var fe kjson.FieldError
		if errors.As(e, &fe) {
			e = fmt.Errorf("%s: unknown field", fe.FieldPath())
		}
		errs = append(errs, e)
	}
	PT(obj).Default()
	if err := PT(obj).Validate(); err != nil {
		errs = append(errs, flatten(err)...)
	}
	if len(errs) == 0 {
		*dst = append(*dst, *obj)
	}
	return errs
}

// flatten splits an errors.Join result into its parts.
func flatten(err error) []error {
	var j interface{ Unwrap() []error }
	if errors.As(err, &j) {
		return j.Unwrap()
	}
	return []error{err}
}

// blank reports whether doc holds nothing but whitespace, comments, and its leading "---"
// line, which the reader keeps.
func blank(doc []byte) bool {
	for line := range bytes.Lines(doc) {
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' && !bytes.HasPrefix(t, []byte("---")) {
			return false
		}
	}
	return true
}

// readFile reads a regular file of at most maxFileSize bytes. Symlinks are followed, since
// Kubernetes ConfigMap volumes are made of them.
func readFile(path string) ([]byte, error) {
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // path is an entry of the operator-controlled config directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("file is larger than %d bytes", maxFileSize)
	}
	return data, nil
}
