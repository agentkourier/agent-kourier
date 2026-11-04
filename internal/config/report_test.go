package config

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// generations gives each object of spec 7's examples a generation, as an API server's objects have, and returns the set.
func generations(t *testing.T) *Set {
	t.Helper()
	set := specSet(t)
	for i := range set.Bindings {
		set.Bindings[i].Generation = 2
	}
	set.AgentBackends[0].Generation = 4
	set.ChatConnections[0].Generation = 6
	return set
}

func drop[T any](items []T, keep func(T) bool) []T {
	return slices.DeleteFunc(slices.Clone(items), func(i T) bool { return !keep(i) })
}

// TestResolvePerObject has a case for each way a source that fails one object at a time says an object is not taken
// as written. What ResolvePartial leaves out for a reference it cannot find, it must name as the object being refused
// when the object exists and is refused.
func TestResolvePerObject(t *testing.T) {
	const backend, conn = "AgentBackend agent-kourier-system/kagent-pilot", "ChatConnection agent-kourier-system/slack-main"
	tests := []struct {
		name       string
		mutate     func(*Set)
		want       []Rejection
		wantServed []string
	}{
		{"nothing refused", func(*Set) {}, nil, []string{"sre/chat-only", "sre/sre-alerts"}},
		{"a reference that is not found keeps its reason and gains the generation",
			func(s *Set) { sreAlerts(s).Spec.Agent.BackendRef.Name = "nope" },
			[]Rejection{{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendNotFound, Generation: 2,
				Message: "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"}},
			[]string{"sre/chat-only"}},
		{"a Binding that was edited into an invalid spec keeps its last good generation",
			func(s *Set) {
				sreAlerts(s).Generation = 2
				s.Invalid = []Invalid{{Kind: KindBinding, Namespace: "sre", Name: "sre-alerts", Generation: 3, Served: 2, Message: "spec.chat.channel: required"}}
			},
			[]Rejection{{Namespace: "sre", Name: "sre-alerts", Reason: ReasonInvalidSpec, Generation: 3, Served: 2,
				Message: "spec: Binding sre/sre-alerts generation 3 is invalid, serving generation 2: spec.chat.channel: required"}},
			[]string{"sre/chat-only", "sre/sre-alerts"}},
		{"a new Binding that is invalid is not served",
			func(s *Set) {
				s.Bindings = drop(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name != "sre-alerts" })
				s.Invalid = []Invalid{{Kind: KindBinding, Namespace: "sre", Name: "sre-alerts", Generation: 1, Message: "spec.chat.channel: required"}}
			},
			[]Rejection{{Namespace: "sre", Name: "sre-alerts", Reason: ReasonInvalidSpec, Generation: 1,
				Message: "spec: Binding sre/sre-alerts generation 1 is invalid, with no earlier generation to serve: spec.chat.channel: required"}},
			[]string{"sre/chat-only"}},
		{"a backend of another namespace that is invalid, has no good generation and allows the Binding is BackendRejected without the owner's text",
			func(s *Set) {
				s.AgentBackends = nil
				s.Invalid = []Invalid{{Kind: KindAgentBackend, Namespace: "agent-kourier-system", Name: "kagent-pilot", Generation: 5, Message: "spec.url: required", AllowedNamespaces: []string{"sre"}}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonBackendRejected, Generation: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, with no earlier generation to serve"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendRejected, Generation: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, with no earlier generation to serve"},
			},
			nil},
		{"a backend of another namespace that is invalid, has no good generation and does not allow the Binding answers as a valid one does",
			func(s *Set) {
				s.AgentBackends = nil
				s.Invalid = []Invalid{{Kind: KindAgentBackend, Namespace: "agent-kourier-system", Name: "kagent-pilot", Generation: 5, Message: "spec.url: required", AllowedNamespaces: []string{"payments"}}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonNamespaceNotAllowed, Generation: 2, Message: "spec.agent.backendRef: " + backend + " does not allow namespace sre"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonNamespaceNotAllowed, Generation: 2, Message: "spec.agent.backendRef: " + backend + " does not allow namespace sre"},
			},
			nil},
		{"a backend in the Binding's own namespace that is invalid has the owner's text in the rejection",
			func(s *Set) {
				sreAlerts(s).Spec.Agent.BackendRef = v1alpha1.ObjectRef{Name: "mine"}
				s.Invalid = []Invalid{{Kind: KindAgentBackend, Namespace: "sre", Name: "mine", Generation: 5, Message: "spec.url: required"}}
			},
			[]Rejection{{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendRejected, Generation: 2,
				Message: "spec.agent.backendRef: AgentBackend sre/mine generation 5 is invalid, with no earlier generation to serve: spec.url: required"}},
			[]string{"sre/chat-only"}},
		{"a backend that is invalid and has a last good generation leaves its Bindings served and flagged, without the owner's text",
			func(s *Set) {
				s.Invalid = []Invalid{{Kind: KindAgentBackend, Namespace: "agent-kourier-system", Name: "kagent-pilot", Generation: 5, Served: 4, Message: "spec.url: required"}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonBackendRejected, Generation: 2, Served: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, serving generation 4"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendRejected, Generation: 2, Served: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, serving generation 4"},
			},
			[]string{"sre/chat-only", "sre/sre-alerts"}},
		{"a connection that is invalid, has no good generation and allows the Binding is ConnectionRejected",
			func(s *Set) {
				s.ChatConnections = nil
				s.Invalid = []Invalid{{Kind: KindChatConnection, Namespace: "agent-kourier-system", Name: "slack-main", Generation: 7, Message: "spec.platform: unsupported", AllowedNamespaces: []string{"sre"}}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonConnectionRejected, Generation: 2,
					Message: "spec.chat.connectionRef: " + conn + " generation 7 is invalid, with no earlier generation to serve"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonConnectionRejected, Generation: 2,
					Message: "spec.chat.connectionRef: " + conn + " generation 7 is invalid, with no earlier generation to serve"},
			},
			nil},
		{"a connection that was deleted and is held says so",
			func(s *Set) {
				s.Invalid = []Invalid{{Kind: KindChatConnection, Namespace: "agent-kourier-system", Name: "slack-main", Served: 6, Deleted: true}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonConnectionRejected, Generation: 2, Served: 2,
					Message: "spec.chat.connectionRef: " + conn + " was deleted, serving generation 6"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonConnectionRejected, Generation: 2, Served: 2,
					Message: "spec.chat.connectionRef: " + conn + " was deleted, serving generation 6"},
			},
			[]string{"sre/chat-only", "sre/sre-alerts"}},
		{"an invalid Binding on an invalid backend has both, its own spec first",
			func(s *Set) {
				s.Invalid = []Invalid{
					{Kind: KindAgentBackend, Namespace: "agent-kourier-system", Name: "kagent-pilot", Generation: 5, Served: 4, Message: "b"},
					{Kind: KindBinding, Namespace: "sre", Name: "chat-only", Generation: 3, Served: 2, Message: "a"},
				}
			},
			[]Rejection{
				{Namespace: "sre", Name: "chat-only", Reason: ReasonInvalidSpec, Generation: 3, Served: 2,
					Message: "spec: Binding sre/chat-only generation 3 is invalid, serving generation 2: a"},
				{Namespace: "sre", Name: "chat-only", Reason: ReasonBackendRejected, Generation: 2, Served: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, serving generation 4"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendRejected, Generation: 2, Served: 2,
					Message: "spec.agent.backendRef: " + backend + " generation 5 is invalid, serving generation 4"},
			},
			[]string{"sre/chat-only", "sre/sre-alerts"}},
		{"a Binding at its last good generation that resolution then drops still has its refused spec, at the new generation",
			func(s *Set) {
				sreAlerts(s).Spec.Agent.BackendRef.Name = "nope"
				s.Invalid = []Invalid{{Kind: KindBinding, Namespace: "sre", Name: "sre-alerts", Generation: 5, Served: 2, Message: "x"}}
			},
			[]Rejection{
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonInvalidSpec, Generation: 5,
					Message: "spec: Binding sre/sre-alerts generation 5 is invalid, and its last good generation 2 is not served either: x"},
				{Namespace: "sre", Name: "sre-alerts", Reason: ReasonBackendNotFound, Generation: 5,
					Message: "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"},
			},
			[]string{"sre/chat-only"}},
		{"a channel conflict is a rejection that is not served",
			func(s *Set) {
				sreAlerts(s).CreationTimestamp = at(2)
				s.Bindings = append(s.Bindings, bindingLike(s, "sre", "older", "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(1); b.Generation = 9 }))
			},
			[]Rejection{{Namespace: "sre", Name: "sre-alerts", Reason: ReasonChannelConflict, Generation: 2,
				Message: "spec.chat.channel: C0123456789 on " + conn + " is already bound by Binding sre/older"}},
			[]string{"sre/chat-only", "sre/older"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := generations(t)
			tc.mutate(set)
			snap, got := resolvePerObject(set)
			if !slices.Equal(got, tc.want) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, tc.want)
			}
			if s := served(snap); !slices.Equal(s, tc.wantServed) {
				t.Errorf("served = %v; want %v", s, tc.wantServed)
			}
		})
	}
}

// Report.Unserved is the Bindings that exist and are not in the snapshot: not those that are flagged and served, and each once.
func TestReportUnserved(t *testing.T) {
	r := Report{Rejections: []Rejection{
		{Namespace: "sre", Name: "b", Reason: ReasonBackendNotFound},
		{Namespace: "sre", Name: "b", Reason: ReasonConnectionNotFound},
		{Namespace: "sre", Name: "a", Reason: ReasonInvalidSpec},
		{Namespace: "sre", Name: "served", Reason: ReasonInvalidSpec, Served: 3},
	}}
	if got := r.Unserved(); !slices.Equal(got, []string{"sre/a", "sre/b"}) {
		t.Errorf("Unserved = %v; want [sre/a sre/b]", got)
	}
}

func TestCapMessage(t *testing.T) {
	if got := CapMessage("short"); got != "short" {
		t.Errorf("CapMessage(short) = %q", got)
	}
	long := strings.Repeat("é", 3000)
	got := CapMessage(long)
	if n := len([]rune(got)); n != maxMessage || !strings.HasSuffix(got, "…") {
		t.Errorf("CapMessage of 3000 characters has %d characters, ends in an ellipsis %v; want %d and true", n, strings.HasSuffix(got, "…"), maxMessage)
	}
}

// perObjectSource hands out the set the test gives it, a fresh copy each time, with a fingerprint it moves by hand.
type perObjectSource struct {
	t    *testing.T
	make func(*testing.T) *Set
	fp   string
}

func (p *perObjectSource) Set(context.Context) (*Set, string, error) { return p.make(p.t), p.fp, nil }
func (p *perObjectSource) Fingerprint(context.Context) string        { return p.fp }

func newPerObjectReloader(src Source, secrets SecretResolver, mutate func(*ReloaderConfig)) *Reloader {
	cfg := ReloaderConfig{Source: src, Secrets: secrets, PerObject: true, Logger: slog.New(slog.DiscardHandler)}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewReloader(cfg)
}

// A Reloader that runs per object publishes a snapshot of what it can serve, where the strict one fails the reload, reads
// no Secret to decide, and follows only the Secrets of what the snapshot serves. The report says what was left out.
func TestReloaderPerObjectServesWhatItCanAndFollowsOnlyItsSecrets(t *testing.T) {
	// sre-alerts names a backend that does not exist and a Secret that does not either; chat-only is fine.
	mk := func(t *testing.T) *Set {
		set := generations(t)
		sreAlerts(set).Spec.Agent.BackendRef.Name = "nope"
		return set
	}
	secrets := &watchingSecrets{fakeSecrets: fakeSecrets{}} // every Secret is missing
	var reports []Report
	r := newPerObjectReloader(&perObjectSource{t: t, make: mk, fp: "v1"}, secrets, func(c *ReloaderConfig) { c.OnReport = func(r Report) { reports = append(reports, r) } })
	if err := r.reload(t.Context(), true); err != nil {
		t.Fatalf("a Secret that is missing failed the reload of a per-object Reloader: %v", err)
	}
	snap := r.Current()
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only"}) {
		t.Errorf("served = %v; want only sre/chat-only", s)
	}
	rep := r.Report()
	if rep.Snapshot != snap || len(rep.Rejections) != 1 || rep.Rejections[0].Name != "sre-alerts" || rep.Rejections[0].Reason != ReasonBackendNotFound {
		t.Errorf("report = %+v; want sre-alerts rejected as BackendNotFound, with the snapshot", rep)
	}
	if len(reports) != 1 || len(reports[0].Rejections) != 1 {
		t.Errorf("OnReport was called with %+v; want once, with the rejection", reports)
	}
	// chat-only has no token Secret; sre-alerts has one, and is rejected. Only the connection's Secret is followed.
	want := []string{"watch [agent-kourier-system/slack-main]", "retain [agent-kourier-system/slack-main]"}
	if got := secrets.take(); !slices.Equal(got, want) {
		t.Errorf("the Secrets asked of the resolver:\n%v\nwant:\n%v", got, want)
	}
	for _, c := range secrets.calls {
		if c == "keys" {
			t.Error("a per-object Reloader read a Secret's keys; it must leave that to the prober")
		}
	}
}

// The revision does not move for a Secret that only a rejected Binding names, and does when that Binding is accepted.
func TestReloaderPerObjectRevisionIgnoresARejectedBindingsSecrets(t *testing.T) {
	versions := newVersionedSecrets()
	broken := true
	mk := func(t *testing.T) *Set {
		set := generations(t)
		if broken {
			sreAlerts(set).Spec.Agent.BackendRef.Name = "nope"
		}
		return set
	}
	src := &perObjectSource{t: t, make: mk, fp: "v1"}
	r := newPerObjectReloader(src, versions, nil)
	if err := r.reload(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	before := r.Revision(t.Context())
	versions.versions["sre/agent-kourier-sre-alerts-oidc"] = "2"
	if after := r.Revision(t.Context()); after != before {
		t.Error("the revision moved for a Secret that only a rejected Binding names")
	}
	broken, src.fp = false, "v2"
	if err := r.reload(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	accepted := r.Revision(t.Context())
	versions.versions["sre/agent-kourier-sre-alerts-oidc"] = "3"
	if after := r.Revision(t.Context()); after == accepted {
		t.Error("the revision did not move for a Secret of a Binding that is served")
	}
}

// A recheck that finds the same rejections says nothing, and a rejection that clears is said once.
func TestReloaderPerObjectLogsEachRejectionOnce(t *testing.T) {
	var log bytes.Buffer
	broken := true
	mk := func(t *testing.T) *Set {
		set := generations(t)
		if broken {
			sreAlerts(set).Spec.Agent.BackendRef.Name = "nope"
		}
		return set
	}
	src := &perObjectSource{t: t, make: mk, fp: "v1"}
	r := newPerObjectReloader(src, specSecrets(), func(c *ReloaderConfig) { c.Logger = slog.New(slog.NewTextHandler(&log, nil)) })
	for range 3 {
		if err := r.reload(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), "a Binding is not accepted as written"); n != 1 {
		t.Errorf("the rejection was logged %d times over three rechecks; want once\n%s", n, log.String())
	}
	broken, src.fp = false, "v2"
	for range 2 {
		if err := r.reload(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), "every object is accepted"); n != 1 {
		t.Errorf("the end of the rejections was logged %d times; want once\n%s", n, log.String())
	}
}

// A file Reloader is not per object: a Binding that cannot be resolved still fails the reload, and its report is empty.
func TestReloaderOfFilesIsNotPerObject(t *testing.T) {
	mk := func(t *testing.T) *Set {
		set := generations(t)
		sreAlerts(set).Spec.Agent.BackendRef.Name = "nope"
		return set
	}
	r := NewReloader(ReloaderConfig{Source: &perObjectSource{t: t, make: mk, fp: "v1"}, Secrets: specSecrets(), Logger: slog.New(slog.DiscardHandler)})
	if err := r.reload(t.Context(), true); err == nil {
		t.Fatal("a Reloader that is not per object published a snapshot with a Binding whose backend is missing")
	}
	if r.Current() != nil || len(r.Report().Rejections) != 0 {
		t.Errorf("a failed first load left a snapshot or a report: %v %+v", r.Current(), r.Report())
	}
}

// The text of another namespace's refused object, a URL with a token in its query, never reaches a Binding's rejection, which
// the Binding's own namespace can read: not when the object has no good generation and is left out, not when it is served
// from an older one, and not for a namespace that it does not allow. The owner's text stays in Invalid.
func TestResolvePerObjectKeepsAnotherNamespacesTextOut(t *testing.T) {
	const text = `spec.url: invalid URL "ftp://internal.team-b.example/?token=s3cret"`
	for _, tc := range []struct {
		name    string
		allowed []string
		served  int64
	}{
		{"allowed, no good generation", []string{"sre"}, 0},
		{"allowed, served from an older generation", []string{"sre"}, 4},
		{"not allowed, no good generation", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := generations(t)
			sreAlerts(set).Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "team-b", Name: "private"}
			set.AgentBackends = set.AgentBackends[:0]
			inv := Invalid{Kind: KindAgentBackend, Namespace: "team-b", Name: "private", Generation: 5, Served: tc.served, Message: text, AllowedNamespaces: tc.allowed}
			set.Invalid = []Invalid{inv}
			if tc.served > 0 {
				be := v1alpha1.AgentBackend{}
				be.Namespace, be.Name, be.Generation, be.Spec.Dialect = "team-b", "private", tc.served, v1alpha1.DialectA2A
				be.Spec.AllowedNamespaces = tc.allowed
				set.AgentBackends = append(set.AgentBackends, be)
			}
			_, got := resolvePerObject(set)
			if len(got) == 0 {
				t.Fatal("no rejection")
			}
			for _, rej := range got {
				if strings.Contains(rej.Message, "s3cret") || strings.Contains(rej.Message, "token") {
					t.Errorf("%s/%s %s: %s; the rejection quotes another namespace's text", rej.Namespace, rej.Name, rej.Reason, rej.Message)
				}
			}
			if !strings.Contains(inv.Message, "s3cret") {
				t.Error("the test lost its token")
			}
		})
	}
}

// The words a Binding gets for a backend of another namespace that is refused and not allowed are those of a valid backend
// that does not allow it: the Binding learns that the object exists, which it learns of a valid one, and no more.
func TestAnInvalidBackendThatDoesNotAllowABindingAnswersAsAValidOneDoes(t *testing.T) {
	valid := generations(t)
	valid.AgentBackends[0].Spec.AllowedNamespaces = []string{"payments"}
	_, want := resolvePerObject(valid)

	refused := generations(t)
	refused.AgentBackends = nil
	refused.Invalid = []Invalid{{Kind: KindAgentBackend, Namespace: "agent-kourier-system", Name: "kagent-pilot", Generation: 5, Message: "spec.url: required", AllowedNamespaces: []string{"payments"}}}
	_, got := resolvePerObject(refused)
	if !slices.Equal(got, want) {
		t.Errorf("rejections for a refused backend:\n%+v\nwant those of a valid one:\n%+v", got, want)
	}
}
