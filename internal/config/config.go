// Package config turns Agent Kourier's resources (AgentBackend, ChatConnection, Binding) into the routing table the broker serves,
// whichever route they came by. Load reads them from YAML files (all or nothing: one bad file fails the load), and DecodeObject
// decodes one object that a custom resource source read from the cluster (internal/config/crsource), tolerating fields it does not
// know. Both run Default and Validate on one object at a time, and the objects are collected in a Set.
//
// Resolve and ResolvePartial then check the references between the objects of a Set (a namespace other than the referrer's needs
// allowedNamespaces, one Binding per channel) and build a Snapshot. Resolve is all or nothing, as the files are, and also requires
// each Secret that an object names to exist with its keys. ResolvePartial leaves out the Bindings that fail and returns a Rejection
// for each, for a source whose objects have different owners; it checks no Secret.
//
// A Reloader polls a Source (a directory of files, or the custom resource source, behind one interface), resolves the Set it reads
// when the source's fingerprint moves or the Secrets may have changed, and publishes the latest good Snapshot, keeping the last one
// when a read or a resolution fails. Alongside each Snapshot it publishes a Report, which names the objects that are not served as written (none for files).
package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// ErrInvalid is wrapped by an error of Load or Resolve that holds a problem the operator must fix in the config: a
// document that does not decode or validate, a reference or Secret that is missing, a directory that does not exist.
// A restart would find the same problem. A directory or file that cannot be read, a Secret that cannot be read and
// a cancellation do not wrap it.
var ErrInvalid = errors.New("the config is invalid")

// invalid marks err as a problem with the config: it wraps ErrInvalid without changing err's text.
type invalid struct{ error }

func (e invalid) Unwrap() error      { return e.error }
func (invalid) Is(target error) bool { return target == ErrInvalid }

// Set holds every object found in a config directory, defaulted and validated, in file then
// document order. References between objects are not resolved. A Set from Load also remembers
// the file and document each object came from, which Resolve names in its problems.
type Set struct {
	AgentBackends   []v1alpha1.AgentBackend
	ChatConnections []v1alpha1.ChatConnection
	Bindings        []v1alpha1.Binding
	// Invalid lists the objects that a source that fails one object at a time refused, which are not in the lists above, or
	// are there as the last good generation of themselves (Invalid.Served). Load and file mode never fill it. Resolve
	// ignores it, and so does ResolvePartial: a Reloader that runs per object reads it (Report).
	Invalid []Invalid

	// sources maps each object to its location, "<file>#<n>" as in Load's problems. It is keyed by
	// identity, so an object added or renamed after Load has the location Load gave its new identity, if any. A Set not
	// made by Load has none.
	sources map[identity]string
}

// Load reads every *.yaml and *.yml file directly inside dir, in lexical order. Each file may
// hold several documents separated by "---". Documents that are empty or only comments are
// skipped and not counted. Each other document must carry the v1alpha1 apiVersion and a known
// kind, decode without unknown fields, and pass Default and Validate. A Binding must also have no status key.
//
// Load reports every problem it finds, joined, each prefixed with "<file>#<n> <kind>
// <namespace>/<name>:" (n is the 1-based document number in the file) and returns a nil Set.
// Resolve uses the same prefix for its problems with the objects of the Set Load returns.
// A missing directory gives an error wrapping fs.ErrNotExist. Each problem with the config wraps ErrInvalid, and so
// does a missing directory.
func Load(ctx context.Context, dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		err = fmt.Errorf("read config directory: %w", err)
		if errors.Is(err, fs.ErrNotExist) {
			err = invalid{err}
		}
		return nil, err
	}
	l := &loader{set: &Set{}, seen: map[identity]string{}}
	for _, e := range entries {
		if !isConfigFile(e.Name()) {
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
	l.set.sources = l.seen
	return l.set, nil
}

// isConfigFile reports whether a directory entry is one Load reads. Dot names are not, which
// leaves out ConfigMap's ..data entries and editors' dangling lock symlinks.
func isConfigFile(name string) bool {
	ext := filepath.Ext(name)
	return !strings.HasPrefix(name, ".") && (ext == ".yaml" || ext == ".yml")
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

// fail records problems found at loc, the location and whatever is known of the object. Each wraps ErrInvalid
// but a file that cannot be read (an *fs.PathError), which is not the config's fault.
func (l *loader) fail(loc string, errs ...error) {
	for _, err := range errs {
		err = fmt.Errorf("%s: %w", loc, err)
		if !errors.As(err, new(*fs.PathError)) {
			err = invalid{err}
		}
		l.errs = append(l.errs, err)
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
		errs = decodeBindingFile(j, &l.set.Bindings)
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
	errs, err := strictDecode(j, obj)
	if err != nil {
		return []error{err}
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

// errStatusInFile is the problem with a Binding file that has a status key.
var errStatusInFile = errors.New("status: a file may not set it, remove it")

// decodeBindingFile is decode for the Binding in a file, which may not set a status. The Binding type has the field,
// so strict decoding accepts it and this refuses it. The refusal is here, not in decode or Validate, because a custom
// resource source will decode the objects of an API server through decode, and they carry a status. Any status key
// is refused, even an empty or null one. It follows the problems decode reports; when decode cannot decode the
// document at all, as with a status of the wrong type, it reports that one problem and the refusal follows it, and
// Validate has not run. Like decode, it adds the Binding to dst when decode found no problem, and Load returns no Set
// when any problem was found.
func decodeBindingFile(j []byte, dst *[]v1alpha1.Binding) []error {
	errs := decode(j, dst)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(j, &top); err != nil {
		return append(errs, err)
	}
	if _, ok := top["status"]; ok {
		errs = append(errs, errStatusInFile)
	}
	return errs
}

// strictDecode decodes the JSON in j into obj and returns each unknown field as an error "path: unknown field".
// The error result is set when j cannot be decoded at all, as with a value of the wrong type.
func strictDecode(j []byte, obj any) (unknown []error, err error) {
	paths, other, err := scanDecode(j, obj)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		unknown = append(unknown, fmt.Errorf("%s: unknown field", p))
	}
	return append(unknown, other...), nil
}

// scanDecode decodes the JSON in j into obj, which holds every field that obj has, and returns the path of each field it
// does not have, in document order, and any other problem the strict decoder found. The error result is set when j cannot
// be decoded at all.
func scanDecode(j []byte, obj any) (paths []string, other []error, err error) {
	strict, err := kjson.UnmarshalStrict(j, obj, kjson.DisallowUnknownFields)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range strict {
		var fe kjson.FieldError
		if errors.As(e, &fe) {
			paths = append(paths, fe.FieldPath())
		} else {
			other = append(other, e)
		}
	}
	// A Duration is decoded from its text, and Validate sees only the result, so the cap on the text is checked here, on the
	// document, for both routes.
	if _, isBinding := obj.(*v1alpha1.Binding); isBinding {
		other = append(other, v1alpha1.DurationTextProblems(j)...)
	}
	return paths, other, nil
}

// DecodeStrict decodes the YAML in data into obj as the loader decodes a resource: a duplicate key is an
// error, and so is every field obj does not have, reported as "path: unknown field". All the problems are
// reported together, joined. It does not default or validate obj. Files that are not resources, such as a
// recipe's evals, use it to refuse the same mistakes.
func DecodeStrict(data []byte, obj any) error {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	unknown, err := strictDecode(j, obj)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return errors.Join(unknown...)
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
