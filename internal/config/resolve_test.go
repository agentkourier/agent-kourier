package config

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// specSet loads spec 7's examples: one backend, one connection, the sre/sre-alerts Binding,
// and a chat-only Binding, sre/chat-only, that has no webhook.
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
		"courier-system/slack-main":   {"botToken", "appToken"},
		"sre/courier-sre-alerts-oidc": {"token"},
		"sre/courier-webhook-token":   {"token"},
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
	set := specSet(t)
	for i := range set.Bindings {
		set.Bindings[i].Status = v1alpha1.BindingStatus{ // status in a file is never input
			WebhookPath: "/hooks/stale",
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		}
	}
	snap, err := Resolve(context.Background(), set, specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	const path = "/hooks/alertmanager/sre/sre-alerts"
	if rb, ok := snap.Binding("sre", "chat-only"); !ok || rb.Binding.Status.WebhookPath != "" || len(rb.Binding.Status.Conditions) != 0 {
		t.Errorf("chat-only Binding = %+v, %v; want it resolved with no webhook path", rb, ok)
	}
	if _, ok := snap.BindingForWebhook("/hooks/stale"); ok {
		t.Error("BindingForWebhook found a path taken from a file")
	}
	lookups := map[string]func() (ResolvedBinding, bool){
		"Binding": func() (ResolvedBinding, bool) { return snap.Binding("sre", "sre-alerts") },
		"BindingForChannel": func() (ResolvedBinding, bool) {
			return snap.BindingForChannel("courier-system", "slack-main", "C0123456789")
		},
		"BindingForWebhook": func() (ResolvedBinding, bool) { return snap.BindingForWebhook(path) },
	}
	for name, lookup := range lookups {
		rb, ok := lookup()
		if !ok || rb.Binding.Name != "sre-alerts" || rb.Backend.Name != "kagent-pilot" || rb.Connection.Name != "slack-main" ||
			rb.Binding.Status.WebhookPath != path || len(rb.Binding.Status.Conditions) != 0 {
			t.Errorf("%s = %+v, %v; want the sre-alerts Binding with its backend and connection", name, rb, ok)
		}
	}
	misses := map[string]func() (ResolvedBinding, bool){
		"Binding": func() (ResolvedBinding, bool) { return snap.Binding("sre", "other") },
		"BindingForChannel": func() (ResolvedBinding, bool) {
			return snap.BindingForChannel("courier-system", "slack-main", "C0000000000")
		},
		"BindingForWebhook": func() (ResolvedBinding, bool) { return snap.BindingForWebhook("/hooks/alertmanager/sre/other") },
	}
	for name, lookup := range misses {
		if _, ok := lookup(); ok {
			t.Errorf("%s found a Binding that does not exist", name)
		}
	}
}

func TestResolveEmptyNamespaceMeansReferrers(t *testing.T) {
	set := specSet(t)
	set.Bindings = slices.DeleteFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Name != "sre-alerts" })
	b, be, c := sreAlerts(set), &set.AgentBackends[0], &set.ChatConnections[0]
	// Same-namespace references need no allowedNamespaces.
	b.Namespace = be.Namespace
	b.Spec.Agent.BackendRef.Namespace, b.Spec.Chat.ConnectionRef.Namespace = "", ""
	c.Spec.DirectMessages.DefaultBindingRef.Namespace = b.Namespace
	be.Spec.AllowedNamespaces, c.Spec.AllowedNamespaces = nil, nil
	secrets := specSecrets()
	secrets["courier-system/courier-sre-alerts-oidc"] = []string{"token"} // Secrets follow the Binding's namespace
	secrets["courier-system/courier-webhook-token"] = []string{"token"}
	snap, err := Resolve(context.Background(), set, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if rb, ok := snap.Binding("courier-system", "sre-alerts"); !ok || rb.Backend != be || rb.Connection != c {
		t.Errorf("Binding = %+v, %v; want it resolved to the backend and connection in its own namespace", rb, ok)
	}
}

func TestResolveRejects(t *testing.T) {
	const from = "Binding sre/sre-alerts: "
	dmFrom := "ChatConnection courier-system/slack-main: spec.directMessages.defaultBindingRef: "
	tests := []struct {
		name   string
		mutate func(*Set)
		want   []string
	}{
		{"missing backend", func(s *Set) { sreAlerts(s).Spec.Agent.BackendRef.Name = "nope" },
			[]string{from + "spec.agent.backendRef: AgentBackend courier-system/nope not found"}},
		{"missing connection", func(s *Set) { sreAlerts(s).Spec.Chat.ConnectionRef.Name = "nope" },
			[]string{from + "spec.chat.connectionRef: ChatConnection courier-system/nope not found"}},
		{"backend does not allow namespace", func(s *Set) { s.AgentBackends[0].Spec.AllowedNamespaces = []string{"payments"} }, []string{
			"Binding sre/chat-only: spec.agent.backendRef: AgentBackend courier-system/kagent-pilot does not allow namespace sre",
			from + "spec.agent.backendRef: AgentBackend courier-system/kagent-pilot does not allow namespace sre",
		}},
		{"connection does not allow namespace", func(s *Set) { s.ChatConnections[0].Spec.AllowedNamespaces = nil }, []string{
			"Binding sre/chat-only: spec.chat.connectionRef: ChatConnection courier-system/slack-main does not allow namespace sre",
			from + "spec.chat.connectionRef: ChatConnection courier-system/slack-main does not allow namespace sre",
		}},
		{"second Binding for a channel", func(s *Set) {
			second := *sreAlerts(s)
			second.Name = "other"
			s.Bindings = append(s.Bindings, second)
		}, []string{"Binding sre/other: spec.chat.channel: C0123456789 on ChatConnection courier-system/slack-main is already bound by Binding sre/sre-alerts"}},
		{"missing default Binding", func(s *Set) { s.ChatConnections[0].Spec.DirectMessages.DefaultBindingRef.Name = "nope" },
			[]string{dmFrom + "Binding sre/nope not found"}},
		{"default Binding uses another connection", func(s *Set) {
			other := s.ChatConnections[0]
			other.Name, other.Spec.DirectMessages = "other-conn", nil
			s.ChatConnections = append(s.ChatConnections, other)
			sreAlerts(s).Spec.Chat.ConnectionRef.Name = "other-conn"
		}, []string{dmFrom + "Binding sre/sre-alerts belongs to ChatConnection courier-system/other-conn, not this one"}},
		{"every problem is reported", func(s *Set) {
			sreAlerts(s).Spec.Agent.BackendRef.Name = "nope"
			s.ChatConnections[0].Spec.DirectMessages.DefaultBindingRef.Name = "nope"
		}, []string{
			from + "spec.agent.backendRef: AgentBackend courier-system/nope not found",
			dmFrom + "Binding sre/nope not found",
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
	other.Name, other.Spec.DirectMessages = "other-conn", nil
	set.ChatConnections = append(set.ChatConnections, other)
	second := *sreAlerts(set)
	second.Name, second.Spec.Chat.ConnectionRef.Name = "other", "other-conn" // same channel ID as sre-alerts
	set.Bindings = append(set.Bindings, second)
	snap, err := Resolve(context.Background(), set, specSecrets())
	if err != nil {
		t.Fatal(err)
	}
	for conn, want := range map[string]string{"slack-main": "sre-alerts", "other-conn": "other"} {
		if rb, ok := snap.BindingForChannel("courier-system", conn, "C0123456789"); !ok || rb.Binding.Name != want {
			t.Errorf("BindingForChannel(%s) = %+v, %v; want %s", conn, rb, ok, want)
		}
	}
	// chat-only's channel is on slack-main only.
	if rb, ok := snap.BindingForChannel("courier-system", "other-conn", "G0123456789"); ok {
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
				a, okA := snap.BindingForChannel("courier-system", "slack-main", "C0123456789")
				b, okB := snap.BindingForWebhook("/hooks/alertmanager/sre/sre-alerts")
				c, okC := snap.Binding("sre", "sre-alerts")
				if !okA || !okB || !okC || a != b || b != c {
					t.Error("lookups disagree")
					return
				}
			}
		})
	}
	wg.Wait()
}
