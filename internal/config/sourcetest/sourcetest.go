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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
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

// Editor builds a Source over the corpus, as a Factory does, and returns it with a function that changes its objects to docs:
// each document of docs that differs from the corpus' is the next generation of the object of its kind, namespace and name.
// The function returns once the source holds the change.
type Editor func(t *testing.T, c Corpus) (src config.Source, change func(docs []string))

// Options says how a source fails.
type Options struct {
	// PerObject means that a bad object is left out of the Set and listed in Set.Invalid while the rest is read, as the
	// custom resources do. Otherwise Set fails with the problems, as the loader of files does, and returns no Set.
	PerObject bool
	// IgnoresUnknownFields means that the source serves an object that has a field the types do not have, and ignores the
	// field, as the custom resources do (the API server's schema decides what an object holds, and a broker may be older
	// than its CRDs). Otherwise the object is refused with the problem "path: unknown field", as the files are.
	IgnoresUnknownFields bool
	// Edit builds a source that the suite then edits, for the cases of an edit. Every source gives one.
	Edit Editor
}

// Run runs the suite against the source that f builds.
func Run(t *testing.T, f Factory, opts Options) {
	if opts.Edit == nil {
		t.Fatal("Options.Edit is nil: the cases of an edit need it")
	}
	spec := Spec(t)
	for _, c := range append([]Corpus{spec}, Recipes(t)...) {
		t.Run("corpus "+c.Name+" reads as the same objects and the same routing table as Load's", func(t *testing.T) { sameObjects(t, f, c) })
	}
	t.Run("the fingerprint is the state that Set read, and holds still", func(t *testing.T) { fingerprintHolds(t, f, spec) })
	t.Run("each Set is the caller's own", func(t *testing.T) { setsAreIndependent(t, f, spec) })
	t.Run("an invalid object is refused with the problem the validator names", func(t *testing.T) { invalidObjects(t, f, spec, opts) })
	t.Run("a rule across objects says the same of the objects of either source", func(t *testing.T) { crossObject(t, f, spec) })
	for _, c := range chatIDCases {
		t.Run(c.name+" is refused as the route refuses a bad object", func(t *testing.T) { refusedChatID(t, f, opts, c) })
		t.Run("an edit to "+c.name+" leaves the Binding as it was", func(t *testing.T) { refusedEdit(t, opts, c) })
	}
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
	{"a channel with a space", "binding-minimal.yaml", "channel: G0123456789", `channel: "sre alerts"`, "spec.chat.channel: invalid channel"},
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

// routing renders the routing table that ResolvePartial makes of set, one line for each Binding and channel, with the output that the
// Binding is served with, which resolution fills in from the platform when the Binding leaves it out.
func routing(set *config.Set) string {
	snap, rejections := config.ResolvePartial(set)
	var b strings.Builder
	for _, rb := range snap.Bindings() {
		fmt.Fprintf(&b, "%s/%s -> %s/%s on %s/%s channel %s output %s\n", rb.Binding.Namespace, rb.Binding.Name, rb.Backend.Namespace,
			rb.Backend.Name, rb.Connection.Namespace, rb.Connection.Name, rb.Binding.Spec.Chat.Channel, rb.Binding.Spec.Chat.Output)
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
		fmt.Fprintf(&b, "%s/%s -> %s/%s on %s/%s channel %s output %s\n", rb.Binding.Namespace, rb.Binding.Name, rb.Backend.Namespace,
			rb.Backend.Name, rb.Connection.Namespace, rb.Connection.Name, rb.Binding.Spec.Chat.Channel, rb.Binding.Spec.Chat.Output)
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
	// chat-only names no output, and is served with the default, live; sre-alerts names live, as a Binding stored by an earlier
	// version does, and keeps it.
	if got := routing(set); !strings.Contains(got, chatOnlyRoute) || !strings.Contains(got, " channel C0123456789 output live\n") || strings.Contains(got, "rejected") {
		t.Errorf("routing table:\n%s\nwant sre/chat-only and sre/sre-alerts routed with output live, and nothing rejected", got)
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
	{"a backend that allows only the Binding's own namespace rejects an agent in another", "agentbackend.yaml", `allowedNamespaces: ["sre", "payments"]`, "allowedNamespaces: [\"sre\", \"payments\"]\n  agents: {sameNamespace: true}",
		config.ReasonAgentNamespaceNotAllowed, "spec.agent.namespace: AgentBackend agent-kourier-system/kagent-pilot does not allow agents in namespace kagent"},
	{"a channel that two Bindings claim", "binding-minimal.yaml", "channel: G0123456789", "channel: C0123456789", config.ReasonChannelConflict,
		"is already bound by Binding sre/"},
}

// chatOnlyRoute is the routing line of the spec corpus' Binding sre/chat-only, which names no output and is served with the default.
const chatOnlyRoute = "sre/chat-only -> agent-kourier-system/kagent-pilot on agent-kourier-system/slack-main channel G0123456789 output live\n"

// chatIDCase is a change to a Binding of the spec corpus that gives it a chat ID that its connection's platform does not take, with
// the reason that the Binding is told and the problem; how a Snapshot serves the field, before the change; and the routing table of
// the rest. The schema and Validate hold each chat ID of a Binding with Slack's triggers to Slack's shape, so Validate refuses each case
// (InvalidSpec) before the broker's check against the platform (InvalidPlatformID, which resolution and the custom resource source
// make) would: TestPlatformIDs and TestRefusedOnPlatform in internal/config cover that check with a table of their own. A Slack ID
// typo in an edit thus does not take, on either route.
type chatIDCase struct {
	name     string
	doc      int    // the Binding's document in the corpus
	binding  string // the Binding's name, in namespace sre
	from, to string
	reason   config.RejectionReason
	problem  string
	served   func(config.ResolvedBinding) string
	before   string
	rest     string
}

var chatIDCases = []chatIDCase{
	{
		name: "a bot's name as a sender", doc: 2, binding: "sre-alerts", rest: chatOnlyRoute,
		from: "from: { id: B0123456789 }", to: "from: { id: alertmanager }",
		reason:  config.ReasonInvalidSpec,
		problem: `spec.chat.triggers[1].from.id: invalid sender ID "alertmanager": must be the sender's ID (Slack: a bot ID such as B0123456789), not a name`,
		served:  func(rb config.ResolvedBinding) string { return rb.Binding.Spec.Chat.Triggers[1].From.ID }, before: "B0123456789",
	},
	{
		name: "a channel's name", doc: 2, binding: "sre-alerts", rest: chatOnlyRoute,
		from: "channel: C0123456789 ", to: "channel: general ",
		reason:  config.ReasonInvalidSpec,
		problem: `spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789, not a name`,
		served:  func(rb config.ResolvedBinding) string { return rb.Binding.Spec.Chat.Channel }, before: "C0123456789",
	},
}

// changed returns docs, the case's corpus, with the case's change made to its Binding.
func (c chatIDCase) changed(t *testing.T, docs []string) []string {
	t.Helper()
	docs = slices.Clone(docs)
	if !strings.Contains(docs[c.doc], c.from) {
		t.Fatalf("the corpus is stale: the Binding sre/%s has no %q", c.binding, c.from)
	}
	docs[c.doc] = strings.Replace(docs[c.doc], c.from, c.to, 1)
	return docs
}

// refusedChatID: a Binding that names a chat ID that its connection's platform does not take is refused, in the same words on either
// route. The files fail, and so the whole load. The custom resources list that Binding in Set.Invalid, with the reason it is told and
// nothing served in its place, and read and serve the rest.
func refusedChatID(t *testing.T, f Factory, opts Options, c chatIDCase) {
	base := Spec(t)
	src := f(t, Corpus{Name: base.Name, Docs: c.changed(t, base.Docs), Connection: base.Connection})
	if !opts.PerObject {
		if _, _, err := src.Set(context.Background()); err == nil || !strings.Contains(err.Error(), c.problem) {
			t.Errorf("Set = %v; want it to fail with the problem %q", err, c.problem)
		}
		return
	}
	set, _ := read(t, src)
	if len(set.Invalid) != 1 {
		t.Fatalf("Invalid = %+v; want sre/%s alone", set.Invalid, c.binding)
	}
	inv := set.Invalid[0]
	if inv.Name != c.binding || cmp.Or(inv.Reason, config.ReasonInvalidSpec) != c.reason || !strings.Contains(inv.Message, c.problem) || inv.Served != 0 {
		t.Errorf("Invalid = %+v; want sre/%s, not served, refused with %s and the problem %q", inv, c.binding, c.reason, c.problem)
	}
	if got := routing(set); got != c.rest {
		t.Errorf("routing table:\n%s\nwant the other Bindings alone, and nothing rejected:\n%s", got, c.rest)
	}
}

// refusedEdit: an edit that gives a serving Binding a chat ID that its platform does not take does not take, on either route. The
// files fail the reload with the problem, and the Reloader keeps the config it serves. The custom resources keep serving the
// Binding's last good generation, and the Report says why the new one is refused, with the reason that the Binding's status carries.
func refusedEdit(t *testing.T, opts Options, c chatIDCase) {
	base := Spec(t)
	docs := base.Docs
	src, change := opts.Edit(t, Corpus{Name: base.Name, Docs: docs, Connection: base.Connection})
	first, _ := read(t, src)
	errs := make(chan error, 16)
	rl := config.NewReloader(config.ReloaderConfig{
		Source: src, Secrets: config.PresentSecrets(first), PollInterval: 10 * time.Millisecond, PerObject: opts.PerObject,
		Logger: slog.New(slog.DiscardHandler), OnError: func(err error) {
			select {
			case errs <- err:
			default:
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); rl.Wait() })
	if err := rl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	served := func() string {
		rb, ok := rl.Current().Binding("sre", c.binding)
		if !ok {
			return "not served"
		}
		return c.served(rb)
	}
	if got := served(); got != c.before {
		t.Fatalf("before the edit sre/%s serves %q; want %q", c.binding, got, c.before)
	}

	change(c.changed(t, docs))
	if !opts.PerObject {
		select {
		case err := <-errs:
			if !strings.Contains(err.Error(), c.problem) {
				t.Errorf("the reload failed with %v; want the problem %q", err, c.problem)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reload did not fail within 5s")
		}
		if got := served(); got != c.before {
			t.Errorf("after the edit sre/%s serves %q; want %q, the config before the edit", c.binding, got, c.before)
		}
		return
	}
	var rej config.Rejection
	testutil.Eventually(t, "the Report rejects sre/"+c.binding, func() bool {
		rejections := rl.Report().Rejections
		i := slices.IndexFunc(rejections, func(r config.Rejection) bool { return r.Namespace == "sre" && r.Name == c.binding })
		if i >= 0 {
			rej = rejections[i]
		}
		return i >= 0
	}, testutil.Within(5*time.Second), testutil.Every(10*time.Millisecond))
	if rej.Reason != c.reason || !strings.Contains(rej.Message, c.problem) || rej.Served == 0 || rej.Generation <= rej.Served {
		t.Errorf("Rejection = %+v; want %s with the problem %q, the new generation refused and an older one served", rej, c.reason, c.problem)
	}
	if got := served(); got != c.before {
		t.Errorf("after the edit sre/%s serves %q; want %q, its last good generation", c.binding, got, c.before)
	}
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
