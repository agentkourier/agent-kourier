// Package sourcetest is the conformance suite of config.Source: what every source of config objects must do the same, run
// by the file source and by the custom resource source, so that the two routes decode, default, validate and resolve one
// object the same way (spec D50). A source differs in how it handles a bad object, which is the failure model of its route,
// and the suite checks the two models as two ways of saying the same problem.
//
// A test that wants a source in the suite gives Run a Factory that builds it over a set of YAML documents.
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

	"github.com/agentkourier/agent-kourier/internal/config"
)

// Factory builds a Source over the documents, each one the manifest of an object: apiVersion, kind, metadata and spec. It
// registers its own cleanup with t. The source is ready to read.
type Factory func(t *testing.T, docs []string) config.Source

// Options says how a source fails.
type Options struct {
	// PerObject means that a bad object is left out of the Set and listed in Set.Invalid while the rest is read, as the
	// custom resources do. Otherwise Set fails with the problems, as the loader of files does, and returns no Set.
	PerObject bool
}

// Run runs the suite against the source that f builds.
func Run(t *testing.T, f Factory, opts Options) {
	t.Run("a valid corpus reads as the same objects and the same routing table", func(t *testing.T) { sameObjects(t, f) })
	t.Run("the fingerprint is the state that Set read, and holds still", func(t *testing.T) { fingerprintHolds(t, f) })
	t.Run("each Set is the caller's own", func(t *testing.T) { setsAreIndependent(t, f) })
	t.Run("an invalid object is refused with the problem the validator names", func(t *testing.T) { invalidObjects(t, f, opts) })
}

// Valid returns the valid corpus, the examples of the API types: one backend, one connection and two Bindings, which refer to
// each other. The objects are the ones in api/v1alpha1/testdata.
func Valid(t *testing.T) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "..", "api", "v1alpha1", "testdata")
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

func read(t *testing.T, src config.Source) (*config.Set, string) {
	t.Helper()
	set, fp, err := src.Set(context.Background())
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	return set, fp
}

// sameObjects: the source gives the objects of the corpus, defaulted, and resolving them gives the routing table that
// resolving what Load gives gives. The two sources are read as they are and compared with what the corpus says it is.
func sameObjects(t *testing.T, f Factory) {
	set, _ := read(t, f(t, Valid(t)))
	want := []string{
		`AgentBackend agent-kourier-system/kagent-pilot `,
		`Binding sre/chat-only `,
		`Binding sre/sre-alerts `,
		`ChatConnection agent-kourier-system/slack-main `,
	}
	got := viewsOf(t, set)
	if len(got) != len(want) {
		t.Fatalf("the source gave %d objects, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("object %d is %q; want one that starts %q", i, got[i], want[i])
		}
	}
	wantRouting := "sre/chat-only -> agent-kourier-system/kagent-pilot on agent-kourier-system/slack-main channel G0123456789\n"
	if got := routing(set); !strings.Contains(got, wantRouting) || strings.Contains(got, "rejected") {
		t.Errorf("routing table:\n%s\nwant it to hold %q and reject nothing", got, wantRouting)
	}
	// What the loader makes of the same documents is the reference: the defaults in the specs are Default's.
	ref := referenceSet(t, Valid(t))
	if a, b := viewsOf(t, set), viewsOf(t, ref); !slices.Equal(a, b) {
		t.Errorf("the objects differ from what Load makes of the same documents:\n%s\nwant:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if a, b := routing(set), routing(ref); a != b {
		t.Errorf("the routing table differs from what Load's objects give:\n%s\nwant:\n%s", a, b)
	}
}

// referenceSet loads docs as files, through config.Load, which every source's objects must equal.
func referenceSet(t *testing.T, docs []string) *config.Set {
	t.Helper()
	dir := t.TempDir()
	for i, d := range docs {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.yaml", i)), []byte(d), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := config.Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func fingerprintHolds(t *testing.T, f Factory) {
	src := f(t, Valid(t))
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
// memory to a second.
func setsAreIndependent(t *testing.T, f Factory) {
	src := f(t, Valid(t))
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
func invalidObjects(t *testing.T, f Factory, opts Options) {
	for _, c := range invalidCases {
		t.Run(c.name, func(t *testing.T) {
			docs, problem := invalid(t, c)
			src := f(t, docs)
			set, fp, err := src.Set(context.Background())
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
