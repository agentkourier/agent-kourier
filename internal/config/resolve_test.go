package config

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// specSet loads spec 7's examples: one backend, one connection, the sre/sre-alerts Binding,
// and a second Binding, sre/chat-only.
func specSet(t *testing.T) *Set {
	t.Helper()
	set, err := Load(context.Background(), "../../api/v1alpha1/testdata")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// sreAlerts returns the Binding that spec 7 shows.
func sreAlerts(s *Set) *v1alpha1.Binding {
	return &s.Bindings[slices.IndexFunc(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "sre-alerts" })]
}

// fakeSecrets maps "namespace/name" to the key names of a Secret.
type fakeSecrets map[string][]string

func (f fakeSecrets) Keys(_ context.Context, namespace, name string) ([]string, error) {
	keys, ok := f[namespace+"/"+name]
	if !ok {
		return nil, fmt.Errorf("get %s/%s: %w", namespace, name, ErrSecretNotFound)
	}
	return keys, nil
}

// specSecrets holds the Secrets that spec 7's examples refer to.
func specSecrets() fakeSecrets {
	return fakeSecrets{
		"agent-kourier-system/slack-main":   {"botToken", "appToken"},
		"sre/agent-kourier-sre-alerts-oidc": {"token"},
	}
}

// wantProblems requires Resolve to return no Snapshot and exactly the given problems.
func wantProblems(t *testing.T, set *Set, secrets SecretResolver, want ...string) {
	t.Helper()
	snap, err := Resolve(context.Background(), set, secrets)
	if err == nil || snap != nil {
		t.Fatalf("Resolve = %+v, %v; want a nil Snapshot and an error", snap, err)
	}
	var got []string
	for _, e := range flatten(err) {
		got = append(got, e.Error())
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestResolveSpecExample(t *testing.T) {
	snap, err := Resolve(context.Background(), specSet(t), specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if rb, ok := snap.Binding("sre", "chat-only"); !ok {
		t.Errorf("chat-only Binding = %+v, %v; want it resolved", rb, ok)
	}
	lookups := map[string]func() (ResolvedBinding, bool){
		"Binding": func() (ResolvedBinding, bool) { return snap.Binding("sre", "sre-alerts") },
		"BindingForChannel": func() (ResolvedBinding, bool) {
			return snap.BindingForChannel("agent-kourier-system", "slack-main", "C0123456789")
		},
	}
	for name, lookup := range lookups {
		rb, ok := lookup()
		if !ok || rb.Binding.Name != "sre-alerts" || rb.Backend.Name != "kagent-pilot" || rb.Connection.Name != "slack-main" {
			t.Errorf("%s = %+v, %v; want the sre-alerts Binding with its backend and connection", name, rb, ok)
		}
	}
	misses := map[string]func() (ResolvedBinding, bool){
		"Binding": func() (ResolvedBinding, bool) { return snap.Binding("sre", "other") },
		"BindingForChannel": func() (ResolvedBinding, bool) {
			return snap.BindingForChannel("agent-kourier-system", "slack-main", "C0000000000")
		},
	}
	for name, lookup := range misses {
		if _, ok := lookup(); ok {
			t.Errorf("%s found a Binding that does not exist", name)
		}
	}
}

// Unknown dialects cannot resolve into a served snapshot.
func TestResolveRejectsABindingOnAnUnbuiltDialect(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.Dialect = "unknown"
	const why = `: spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot uses the "unknown" dialect, supported dialects are "kagent-v1" and "a2a"`
	wantProblems(t, set, specSecrets(), "Binding sre/chat-only"+why, "Binding sre/sre-alerts"+why)

	set = specSet(t) // the dialect that is built still resolves
	if _, err := Resolve(context.Background(), set, specSecrets()); err != nil {
		t.Errorf("a kagent-v1 Binding failed to resolve: %v", err)
	}
}

func TestResolveEmptyNamespaceMeansReferrers(t *testing.T) {
	set := specSet(t)
	set.Bindings = slices.DeleteFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Name != "sre-alerts" })
	b, be, c := sreAlerts(set), &set.AgentBackends[0], &set.ChatConnections[0]
	// Same-namespace references need no allowedNamespaces.
	b.Namespace = be.Namespace
	b.Spec.Agent.BackendRef.Namespace, b.Spec.Chat.ConnectionRef.Namespace = "", ""
	be.Spec.AllowedNamespaces, c.Spec.AllowedNamespaces = nil, nil
	secrets := specSecrets()
	secrets["agent-kourier-system/agent-kourier-sre-alerts-oidc"] = []string{"token"} // Secrets follow the Binding's namespace
	snap, err := Resolve(context.Background(), set, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if rb, ok := snap.Binding("agent-kourier-system", "sre-alerts"); !ok || rb.Backend != be || rb.Connection != c {
		t.Errorf("Binding = %+v, %v; want it resolved to the backend and connection in its own namespace", rb, ok)
	}
}

func TestResolveRejects(t *testing.T) {
	const from = "Binding sre/sre-alerts: "
	tests := []struct {
		name   string
		mutate func(*Set)
		want   []string
	}{
		{"missing backend", func(s *Set) { sreAlerts(s).Spec.Agent.BackendRef.Name = "nope" },
			[]string{from + "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"}},
		{"missing connection", func(s *Set) { sreAlerts(s).Spec.Chat.ConnectionRef.Name = "nope" },
			[]string{from + "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found"}},
		{"backend does not allow namespace", func(s *Set) { s.AgentBackends[0].Spec.AllowedNamespaces = []string{"payments"} }, []string{
			"Binding sre/chat-only: spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot does not allow namespace sre",
			from + "spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot does not allow namespace sre",
		}},
		{"connection does not allow namespace", func(s *Set) { s.ChatConnections[0].Spec.AllowedNamespaces = nil }, []string{
			"Binding sre/chat-only: spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main does not allow namespace sre",
			from + "spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main does not allow namespace sre",
		}},
		{"second Binding for a channel", func(s *Set) {
			second := *sreAlerts(s)
			second.Name = "other"
			s.Bindings = append(s.Bindings, second)
		}, []string{"Binding sre/other: spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/sre-alerts"}},
		{"every problem is reported", func(s *Set) {
			sreAlerts(s).Spec.Agent.BackendRef.Name = "nope"
			sreAlerts(s).Spec.Chat.ConnectionRef.Name = "nope"
		}, []string{
			from + "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found",
			from + "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := specSet(t)
			tc.mutate(set)
			wantProblems(t, set, specSecrets(), tc.want...)
		})
	}
}

// TestResolveChannelsBelongToConnection: a channel ID may be used once per connection, and a
// lookup names the connection.
func TestResolveChannelsBelongToConnection(t *testing.T) {
	set := specSet(t)
	other := set.ChatConnections[0]
	other.Name = "other-conn"
	set.ChatConnections = append(set.ChatConnections, other)
	second := *sreAlerts(set)
	second.Name, second.Spec.Chat.ConnectionRef.Name = "other", "other-conn" // same channel ID as sre-alerts
	set.Bindings = append(set.Bindings, second)
	snap, err := Resolve(context.Background(), set, specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	for conn, want := range map[string]string{"slack-main": "sre-alerts", "other-conn": "other"} {
		if rb, ok := snap.BindingForChannel("agent-kourier-system", conn, "C0123456789"); !ok || rb.Binding.Name != want {
			t.Errorf("BindingForChannel(%s) = %+v, %v; want %s", conn, rb, ok, want)
		}
	}
	// chat-only's channel is on slack-main only.
	if rb, ok := snap.BindingForChannel("agent-kourier-system", "other-conn", "G0123456789"); ok {
		t.Errorf("BindingForChannel found %+v on the wrong connection", rb)
	}
}

// TestSnapshotConcurrentLookups is for the race detector: lookups share the Snapshot's data.
func TestSnapshotConcurrentLookups(t *testing.T) {
	snap, err := Resolve(context.Background(), specSet(t), specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				a, okA := snap.BindingForChannel("agent-kourier-system", "slack-main", "C0123456789")
				b, okB := snap.Binding("sre", "sre-alerts")
				if !okA || !okB || a != b {
					t.Error("lookups disagree")
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestSnapshotListsEveryConnection: the list holds each ChatConnection, whether or not a
// Binding uses it, sorted by namespace and name, so that a Slack client is started for each.
func TestSnapshotListsEveryConnection(t *testing.T) {
	set := specSet(t)
	for _, name := range []string{"zzz-idle", "aaa-idle"} { // no Binding uses them
		c := set.ChatConnections[0]
		c.Name = name
		set.ChatConnections = append(set.ChatConnections, c)
	}
	snap, err := Resolve(context.Background(), set, specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range snap.ChatConnections() {
		got = append(got, c.Namespace+"/"+c.Name)
	}
	want := []string{"agent-kourier-system/aaa-idle", "agent-kourier-system/slack-main", "agent-kourier-system/zzz-idle"}
	if !slices.Equal(got, want) {
		t.Errorf("ChatConnections = %v; want %v", got, want)
	}
}

// Bindings lists every Binding of the config, sorted by namespace and name, so that a caller that needs them all (the
// retention of sessions, which reads each Binding's threadTTL) does not have to know their names.
func TestSnapshotBindings(t *testing.T) {
	set := specSet(t)
	slices.Reverse(set.Bindings) // listed in the order they were given in, which is not the order of names
	snap, err := Resolve(context.Background(), set, specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rb := range snap.Bindings() {
		got = append(got, rb.Binding.Namespace+"/"+rb.Binding.Name)
	}
	if want := []string{"sre/chat-only", "sre/sre-alerts"}; !slices.Equal(got, want) {
		t.Errorf("Bindings = %v; want %v", got, want)
	}
}
