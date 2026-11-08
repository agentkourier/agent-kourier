// Package sourcetest is the conformance suite of config.Source: what every source of config objects must do the same, run
// by the file source and by the custom resource source, so that the two routes decode, default, validate and resolve one
// object the same way (spec D50). A source differs in how it handles a bad object, which is the failure model of its route,
// and the suite checks the two models as two ways of saying the same problem. It differs in one thing more, which Options
// names: whether a field that the types do not have is a problem or is ignored.
//
// A test that wants a source in the suite gives Run a Factory that builds it over a corpus: a set of YAML documents and the
// ChatConnection that the source is to serve. The corpora are the API's examples and each recipe's, and the suite compares what
// each source reads with what Load makes of the same documents, and what resolving them does, strictly (as the file route
// does) and object by object (as the custom resources do), for the cross-object rules too.
//
// The custom resource source runs the suite over a fake dynamic client, which has no schema. An API server with the CRDs refuses
// most of the invalid cases at apply, by a marker or a CEL rule, and only what the schema cannot say (a rule that stays
// broker-only) reaches the broker; test/crdcorpus is what says which is which. The suite asks of the source what it does with
// an object that reached it, whatever let it through.
package sourcetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// Corpus is a set of objects and the connection that a source of them serves.
type Corpus struct {
	// Name says where the corpus is from.
	Name string
	// Docs are the manifests of the objects, each one document: apiVersion, kind, metadata and spec.
	Docs []string
	// Connection is the ChatConnection of the corpus, which a source that serves one connection is told to serve.
	Connection v1alpha1.ObjectRef
}

// Factory builds a Source over the corpus. It registers its own cleanup with t. The source is ready to read.
type Factory func(t *testing.T, c Corpus) config.Source

// Options says how a source fails.
type Options struct {
	// PerObject means that a bad object is left out of the Set and listed in Set.Invalid while the rest is read, as the
	// custom resources do. Otherwise Set fails with the problems, as the loader of files does, and returns no Set.
	PerObject bool
	// IgnoresUnknownFields means that the source serves an object that has a field the types do not have, and ignores the
	// field, as the custom resources do (the API server's schema decides what an object holds, and a broker may be older
	// than its CRDs). Otherwise the object is refused with the problem "path: unknown field", as the files are.
	IgnoresUnknownFields bool
}

// Run runs the suite against the source that f builds.
func Run(t *testing.T, f Factory, opts Options) {
	spec := Spec(t)
	for _, c := range append([]Corpus{spec}, Recipes(t)...) {
		t.Run("corpus "+c.Name+" reads as the same objects and the same routing table as Load's", func(t *testing.T) { sameObjects(t, f, c) })
	}
	t.Run("the fingerprint is the state that Set read, and holds still", func(t *testing.T) { fingerprintHolds(t, f, spec) })
	t.Run("each Set is the caller's own", func(t *testing.T) { setsAreIndependent(t, f, spec) })
	t.Run("an invalid object is refused with the problem the validator names", func(t *testing.T) { invalidObjects(t, f, spec, opts) })
	t.Run("a rule across objects says the same of the objects of either source", func(t *testing.T) { crossObject(t, f, spec) })
}

// Spec returns the corpus of the API's examples: one backend, one connection and two Bindings, which refer to each other. The
// objects are the ones in api/v1alpha1/testdata.
func Spec(t *testing.T) Corpus {
	return Corpus{Name: "spec", Docs: Valid(t), Connection: v1alpha1.ObjectRef{Namespace: ConnectionNamespace, Name: ConnectionName}}
}

// Recipes returns one corpus for each recipe of examples/recipes: its AgentBackend, ChatConnection and Binding, from its
// kourier directory.
func Recipes(t *testing.T) []Corpus {
	t.Helper()
	dir := filepath.Join(repoRoot(), "examples", "recipes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []Corpus
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var docs []string
		for _, name := range []string{"agentbackend.yaml", "chatconnection.yaml", "binding.yaml"} {
			b, err := os.ReadFile(filepath.Join(dir, e.Name(), "kourier", name)) //nolint:gosec // the repository's own recipes, found from this file
			if err != nil {
				t.Fatal(err)
			}
			docs = append(docs, string(b))
		}
		ref := referenceSet(t, docs)
		if len(ref.ChatConnections) != 1 {
			t.Fatalf("recipe %s has %d ChatConnections; want one", e.Name(), len(ref.ChatConnections))
		}
		out = append(out, Corpus{Name: "recipe " + e.Name(), Docs: docs,
			Connection: v1alpha1.ObjectRef{Namespace: ref.ChatConnections[0].Namespace, Name: ref.ChatConnections[0].Name}})
	}
	if len(out) == 0 {
		t.Fatal("no recipe found under examples/recipes")
	}
	return out
}

// repoRoot is the repository's root, found from this file.
func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "..")
}

// Valid returns the valid corpus, the examples of the API types: one backend, one connection and two Bindings, which refer to
// each other. The objects are the ones in api/v1alpha1/testdata.
func Valid(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(), "api", "v1alpha1", "testdata")
	var docs []string
	for _, name := range []string{"agentbackend.yaml", "chatconnection.yaml", "binding.yaml", "binding-minimal.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // dir is the repository's own testdata, found from this file; name is one of four constants
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(b))
	}
	return docs
}

// Connection is the namespace and name of the connection of the valid corpus, which a source that serves one connection
// is told to serve.
const (
	ConnectionNamespace = "agent-kourier-system"
	ConnectionName      = "slack-main"
)

// An invalid object is a document of the valid corpus, by name, with one change that Validate or the strict decoder
// refuses, and the text of the problem they report.
type invalidCase struct {
	name, file string
	from, to   string
	problem    string
}

var invalidCases = []invalidCase{
	{"a channel that is a name", "binding-minimal.yaml", "channel: G0123456789", `channel: "#general"`, "spec.chat.channel: invalid channel"},
	{"a field the type does not have", "binding.yaml", "  chat:\n", "  chat:\n    chanel: C0123456789\n", "chanel: unknown field"},
	{"a URL with no scheme", "agentbackend.yaml", "url: http://kagent-controller.kagent-pilot:8083", "url: kagent-controller", "spec.url: invalid URL"},
	{"a platform that is not supported", "chatconnection.yaml", "platform: slack", "platform: teams", "spec.platform: unsupported value"},
	{"a dialect that is not supported", "agentbackend.yaml", "dialect: kagent-v1", "dialect: nope", "spec.dialect: unsupported value"},
}

// Invalid returns the valid corpus with the case's change made to the document it names, and the problem that must be
// reported.
func invalid(t *testing.T, c invalidCase) (docs []string, problem string) {
	t.Helper()
	names := map[string]int{"agentbackend.yaml": 0, "chatconnection.yaml": 1, "binding.yaml": 2, "binding-minimal.yaml": 3}
	docs = Valid(t)
	i := names[c.file]
	if !strings.Contains(docs[i], c.from) {
		t.Fatalf("the corpus case %q is stale: %s has no %q", c.name, c.file, c.from)
	}
	docs[i] = strings.Replace(docs[i], c.from, c.to, 1)
	return docs, c.problem
}

// viewsOf renders each object as the suite compares it: what the object is and its spec, and nothing of its metadata, which
// a cluster adds to and a file lacks.
func viewsOf(t *testing.T, set *config.Set) []string {
	t.Helper()
	var out []string
	add := func(kind, ns, name string, spec any) {
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s/%s %s", kind, ns, name, b))
	}
	for _, o := range set.AgentBackends {
		add(config.KindAgentBackend, o.Namespace, o.Name, o.Spec)
	}
	for _, o := range set.ChatConnections {
		add(config.KindChatConnection, o.Namespace, o.Name, o.Spec)
	}
	for _, o := range set.Bindings {
		add(config.KindBinding, o.Namespace, o.Name, o.Spec)
	}
	slices.Sort(out)
	return out
}

// routing renders the routing table that ResolvePartial makes of set, one line for each Binding and channel.
func routing(set *config.Set) string {
	snap, rejections := config.ResolvePartial(set)
	var b strings.Builder
	for _, rb := range snap.Bindings() {
		fmt.Fprintf(&b, "%s/%s -> %s/%s on %s/%s channel %s\n", rb.Binding.Namespace, rb.Binding.Name, rb.Backend.Namespace, rb.Backend.Name,
			rb.Connection.Namespace, rb.Connection.Name, rb.Binding.Spec.Chat.Channel)
	}
	for _, r := range rejections {
		fmt.Fprintf(&b, "rejected %s/%s %s\n", r.Namespace, r.Name, r.Reason)
	}
	return b.String()
}

// routingStrict renders the routing table that Resolve makes of set, which is what the file route builds, with every Secret
// present; and the problem it reports if it reports one. Resolve takes the Set: give it one of its own.
func routingStrict(t *testing.T, set *config.Set) string {
	t.Helper()
	snap, err := config.Resolve(context.Background(), set, config.PresentSecrets(set))
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, rb := range snap.Bindings() {
		fmt.Fprintf(&b, "%s/%s -> %s/%s on %s/%s channel %s\n", rb.Binding.Namespace, rb.Binding.Name, rb.Backend.Namespace, rb.Backend.Name,
			rb.Connection.Namespace, rb.Connection.Name, rb.Binding.Spec.Chat.Channel)
	}
	return b.String()
}

func read(t *testing.T, src config.Source) (*config.Set, string) {
	t.Helper()
	set, fp, err := src.Set(context.Background())
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	return set, fp
}

// sameObjects: the source gives the objects of the corpus, defaulted, as Load gives them, and resolving them gives the routing
// table that resolving what Load gives gives, both strictly and object by object. The spec corpus also says what it holds.
func sameObjects(t *testing.T, f Factory, c Corpus) {
	src := f(t, c)
	set, _ := read(t, src)
	ref := referenceSet(t, c.Docs)
	if a, b := viewsOf(t, set), viewsOf(t, ref); !slices.Equal(a, b) {
		t.Errorf("the objects differ from what Load makes of the same documents:\n%s\nwant:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if a, b := routing(set), routing(referenceSet(t, c.Docs)); a != b {
		t.Errorf("the routing table differs from what Load's objects give:\n%s\nwant:\n%s", a, b)
	}
	strict, _ := read(t, src)
	got, want := routingStrict(t, strict), routingStrict(t, referenceSet(t, c.Docs))
	if got != want || strings.HasPrefix(got, "error:") {
		t.Errorf("strict routing table:\n%s\nwant the same as Load's, which is:\n%s", got, want)
	}
	if c.Name != "spec" {
		return
	}
	for i, w := range []string{
		`AgentBackend agent-kourier-system/kagent-pilot `, `Binding sre/chat-only `, `Binding sre/sre-alerts `, `ChatConnection agent-kourier-system/slack-main `,
	} {
		if got := viewsOf(t, set); len(got) != 4 || !strings.HasPrefix(got[i], w) {
			t.Fatalf("objects = %v; want four, the %dth starting %q", got, i, w)
		}
	}
	if got := routing(set); !strings.Contains(got, "sre/chat-only -> agent-kourier-system/kagent-pilot on agent-kourier-system/slack-main channel G0123456789\n") || strings.Contains(got, "rejected") {
		t.Errorf("routing table:\n%s\nwant sre/chat-only routed and nothing rejected", got)
	}
}

// referenceSet loads docs as files, through config.Load, which every source's objects must equal.
func referenceSet(t *testing.T, docs []string) *config.Set {
	t.Helper()
	dir := t.TempDir()
	for i, d := range docs {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.yaml", i)), []byte(d), 0o600); err != nil { //nolint:gosec // dir is t.TempDir(); the name is an index
			t.Fatal(err)
		}
	}
	set, err := config.Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func fingerprintHolds(t *testing.T, f Factory, c Corpus) {
	src := f(t, c)
	_, fp := read(t, src)
	if fp == "" {
		t.Error("Set returned an empty fingerprint")
	}
	if now := src.Fingerprint(context.Background()); now != fp {
		t.Errorf("Fingerprint = %q after Set returned %q, with nothing changed", now, fp)
	}
	if _, again := read(t, src); again != fp {
		t.Errorf("a second Set returned fingerprint %q; want %q", again, fp)
	}
}

// setsAreIndependent: resolving takes ownership of a Set and reorders its Bindings, so a source must never hand one Set's
// slices to a second, and a change of the fields of one Set's objects must not show in another. What those fields hold (a slice
// or a map) may be shared, and is for nobody to change.
func setsAreIndependent(t *testing.T, f Factory, c Corpus) {
	src := f(t, c)
	first, _ := read(t, src)
	want := viewsOf(t, first)
	first.Bindings[0].Spec.Chat.Channel = "G9999999999"
	first.AgentBackends[0].Spec.URL = "http://changed.example"
	first.ChatConnections[0].Spec.AllowedNamespaces = []string{"changed"}
	second, _ := read(t, src)
	if got := viewsOf(t, second); !slices.Equal(got, want) {
		t.Errorf("changing the first Set changed the second:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// invalidObjects: every case is refused with the problem the validator names. A source that fails per object leaves the
// object out, lists it in Set.Invalid with the same problem, and reads the rest; one that does not fails the whole read with
// the problem.
func invalidObjects(t *testing.T, f Factory, base Corpus, opts Options) {
	for _, c := range invalidCases {
		t.Run(c.name, func(t *testing.T) {
			docs, problem := invalid(t, c)
			src := f(t, Corpus{Name: base.Name, Docs: docs, Connection: base.Connection})
			set, fp, err := src.Set(context.Background())
			if opts.IgnoresUnknownFields && strings.HasSuffix(problem, ": unknown field") {
				if err != nil || len(set.Invalid) != 0 || len(viewsOf(t, set)) != 4 {
					t.Fatalf("Set = %d objects, Invalid %+v, error %v; want all four objects, none refused, for a field the type does not have", len(viewsOf(t, set)), set.Invalid, err)
				}
				return
			}
			if fp == "" {
				t.Error("Set returned no fingerprint with the problem, which a Reloader needs to not try the same state again")
			}
			if !opts.PerObject {
				if err == nil || !strings.Contains(err.Error(), problem) {
					t.Fatalf("Set error = %v; want one that holds %q", err, problem)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set: %v; a bad object must not fail the read", err)
			}
			if len(set.Invalid) != 1 || !strings.Contains(set.Invalid[0].Message, problem) || set.Invalid[0].Served != 0 {
				t.Fatalf("Invalid = %+v; want one object, not served, whose problem holds %q", set.Invalid, problem)
			}
			if n := len(viewsOf(t, set)); n != 3 {
				t.Errorf("the Set has %d objects; want the three that are valid", n)
			}
		})
	}
}

// A cross-object case is a document of the spec corpus with one change that breaks a rule across objects: a reference that is not
// found, a namespace that is not allowed, a channel that two Bindings claim. The rule is no problem of any one object, so
// both sources read every object; resolving says what is wrong, strictly with a problem (the file route fails the load with
// it) and object by object with a rejection of the Binding (the custom resources leave it out), in the same words.
type crossCase struct {
	name, file string
	from, to   string
	reason     config.RejectionReason
	problem    string
}

var crossCases = []crossCase{
	{"a backend that is not found", "binding-minimal.yaml", "name: kagent-pilot", "name: nope", config.ReasonBackendNotFound,
		"spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"},
	{"a connection that is not found", "binding-minimal.yaml", "name: slack-main", "name: nope", config.ReasonConnectionNotFound,
		"spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found"},
	{"a namespace that the backend does not allow", "agentbackend.yaml", `allowedNamespaces: ["sre", "payments"]`, `allowedNamespaces: ["payments"]`,
		config.ReasonNamespaceNotAllowed, "AgentBackend agent-kourier-system/kagent-pilot does not allow namespace sre"},
	{"a namespace that the connection does not allow", "chatconnection.yaml", `allowedNamespaces: ["sre", "payments"]`, `allowedNamespaces: ["payments"]`,
		config.ReasonNamespaceNotAllowed, "ChatConnection agent-kourier-system/slack-main does not allow namespace sre"},
	{"an agent namespace that the backend does not list", "agentbackend.yaml", `allowedNamespaces: ["sre", "payments"]`, "allowedNamespaces: [\"sre\", \"payments\"]\n  agents: {namespaces: [payments]}",
		config.ReasonAgentNamespaceNotAllowed, "spec.agent.namespace: AgentBackend agent-kourier-system/kagent-pilot does not allow agents in namespace kagent"},
	{"an agent namespace that is only the Binding's own is allowed", "agentbackend.yaml", `allowedNamespaces: ["sre", "payments"]`, "allowedNamespaces: [\"sre\", \"payments\"]\n  agents: {sameNamespace: true}",
		config.ReasonAgentNamespaceNotAllowed, "spec.agent.namespace: AgentBackend agent-kourier-system/kagent-pilot does not allow agents in namespace kagent"},
	{"a channel that two Bindings claim", "binding-minimal.yaml", "channel: G0123456789", "channel: C0123456789", config.ReasonChannelConflict,
		"is already bound by Binding sre/"},
}

func crossObject(t *testing.T, f Factory, base Corpus) {
	for _, c := range crossCases {
		t.Run(c.name, func(t *testing.T) {
			docs := Valid(t)
			i := map[string]int{"agentbackend.yaml": 0, "chatconnection.yaml": 1, "binding.yaml": 2, "binding-minimal.yaml": 3}[c.file]
			if !strings.Contains(docs[i], c.from) {
				t.Fatalf("the corpus case %q is stale: %s has no %q", c.name, c.file, c.from)
			}
			docs[i] = strings.Replace(docs[i], c.from, c.to, 1)
			src := f(t, Corpus{Name: base.Name, Docs: docs, Connection: base.Connection})

			strict, _ := read(t, src)
			if got := routingStrict(t, strict); !strings.HasPrefix(got, "error:") || !strings.Contains(got, c.problem) {
				t.Errorf("strict resolution = %q; want the problem %q", got, c.problem)
			}
			partial, _ := read(t, src)
			if len(partial.Invalid) != 0 {
				t.Fatalf("Invalid = %+v; a rule across objects is no problem of one object", partial.Invalid)
			}
			_, rejections := config.ResolvePartial(partial)
			found := false
			for _, r := range rejections {
				found = found || (r.Reason == c.reason && strings.Contains(r.Message, c.problem))
			}
			if !found {
				t.Errorf("rejections = %+v; want one for %s that holds %q", rejections, c.reason, c.problem)
			}
		})
	}
}
