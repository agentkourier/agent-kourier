package config

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

func TestResolveSecrets(t *testing.T) {
	const oidc = atSREAlerts + "Binding sre/sre-alerts: spec.identity.tokenSecretRef: "
	const creds = atConn + "ChatConnection agent-kourier-system/slack-main: spec.credentialsSecretRef: "
	tests := []struct {
		name   string
		mutate func(*Set, fakeSecrets)
		want   []string
	}{
		{"missing connection Secret", func(_ *Set, f fakeSecrets) { delete(f, "agent-kourier-system/slack-main") },
			[]string{creds + "Secret agent-kourier-system/slack-main not found"}},
		{"Socket Mode needs appToken", func(_ *Set, f fakeSecrets) {
			f["agent-kourier-system/slack-main"] = []string{"botToken"}
		},
			[]string{creds + `Secret agent-kourier-system/slack-main has no key "appToken"`}},
		{"botToken is needed", func(_ *Set, f fakeSecrets) { f["agent-kourier-system/slack-main"] = []string{"appToken"} },
			[]string{creds + `Secret agent-kourier-system/slack-main has no key "botToken"`}},
		{"identity key is the ref's key", func(s *Set, _ fakeSecrets) { sreAlerts(s).Spec.Identity.TokenSecretRef.Key = "jwt" },
			[]string{oidc + `Secret sre/agent-kourier-sre-alerts-oidc has no key "jwt"`}},
		{"Secrets are read in the Binding's namespace", func(_ *Set, f fakeSecrets) {
			f["payments/agent-kourier-sre-alerts-oidc"] = f["sre/agent-kourier-sre-alerts-oidc"]
			delete(f, "sre/agent-kourier-sre-alerts-oidc")
		}, []string{oidc + "Secret sre/agent-kourier-sre-alerts-oidc not found"}},
		{"header Secret is read in the backend's namespace", func(s *Set, f fakeSecrets) {
			s.AgentBackends[0].Spec.Headers = []v1alpha1.Header{{Name: "X-Team", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "team", Key: "value"}}}
			f["agent-kourier-system/team"] = []string{"token"}
		}, []string{atBackend + `AgentBackend agent-kourier-system/kagent-pilot: spec.headers[0].valueSecretRef: Secret agent-kourier-system/team has no key "value"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, secrets := specSet(t), specSecrets()
			tc.mutate(set, secrets)
			wantProblems(t, set, secrets, tc.want...)
		})
	}

	// signingSecret was the key of the HTTP mode that was never served. It is not a known key now, and a Secret
	// that still carries it, as one made for that mode would, is not a problem: only the keys that are needed are checked.
	t.Run("an unknown key in the Secret is ignored", func(t *testing.T) {
		set, secrets := specSet(t), specSecrets()
		secrets["agent-kourier-system/slack-main"] = []string{"botToken", "appToken", "signingSecret"}
		if _, err := Resolve(context.Background(), set, secrets); err != nil {
			t.Error(err)
		}
	})
}

// failingSecrets fails every read the way an unreachable API server would.
type failingSecrets struct{}

func (failingSecrets) Keys(context.Context, string, string) ([]string, error) {
	return nil, errors.New("connection refused")
}

func TestResolveSecretReadErrors(t *testing.T) {
	wantProblems(t, specSet(t), failingSecrets{},
		// Grouped by object, in the order the checks ran within each.
		atSREAlerts+"Binding sre/sre-alerts: spec.identity.tokenSecretRef: read Secret sre/agent-kourier-sre-alerts-oidc: connection refused",
		atConn+"ChatConnection agent-kourier-system/slack-main: spec.credentialsSecretRef: read Secret agent-kourier-system/slack-main: connection refused",
	)
}

// cancelingSecrets cancels the context on its first read, as a shutdown would mid-check.
type cancelingSecrets struct {
	cancel func()
	reads  int
}

func (c *cancelingSecrets) Keys(ctx context.Context, _, _ string) ([]string, error) {
	c.reads++
	c.cancel()
	return nil, ctx.Err()
}

func TestResolveStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	secrets := &cancelingSecrets{cancel: cancel}
	set := specSet(t)
	sreAlerts(set).Spec.Agent.BackendRef.Name = "nope" // a real problem must survive the cancellation
	_, err := Resolve(ctx, set, secrets)
	got := flatten(err)
	if !errors.Is(err, context.Canceled) || len(got) != 2 || !strings.HasSuffix(got[1].Error(), "AgentBackend agent-kourier-system/nope not found") || secrets.reads != 1 {
		t.Errorf("Resolve error = %v after %d reads; want context.Canceled once, then the reference problem, after 1 read", err, secrets.reads)
	}
}

// readNamespaces records the namespace of every Secret Resolve asks about.
type readNamespaces struct{ seen map[string]bool }

func (r *readNamespaces) Keys(_ context.Context, namespace, _ string) ([]string, error) {
	r.seen[namespace] = true
	return []string{"token", "botToken", "appToken", "value"}, nil
}

// SecretNamespaces and the checks Resolve makes walk the same references, so a namespace that
// Resolve reads is always one the resolver was told to watch.
func TestSecretNamespacesAreTheNamespacesResolveReads(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.Headers = []v1alpha1.Header{{Name: "X-Team", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "team", Key: "value"}}}
	want := set.SecretNamespaces()
	reads := &readNamespaces{seen: map[string]bool{}}
	if _, err := Resolve(context.Background(), set, reads); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(reads.seen)); !slices.Equal(got, want) || len(want) < 2 {
		t.Errorf("Resolve read namespaces %v, SecretNamespaces says %v", got, want)
	}
}

// Only an object that refers to a Secret needs its namespace watched: every ChatConnection does,
// a backend does for a header Secret, a Binding for its token.
func TestSecretNamespaces(t *testing.T) {
	ref := &v1alpha1.SecretKeyRef{Name: "s"}
	meta := func(ns string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: "x"} }
	set := &Set{
		AgentBackends: []v1alpha1.AgentBackend{
			{ObjectMeta: meta("be-secret"), Spec: v1alpha1.AgentBackendSpec{Headers: []v1alpha1.Header{{Name: "A", ValueSecretRef: ref}}}},
			{ObjectMeta: meta("be-plain"), Spec: v1alpha1.AgentBackendSpec{Headers: []v1alpha1.Header{{Name: "A", Value: "v"}}}},
		},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("conn")}},
		Bindings: []v1alpha1.Binding{
			{ObjectMeta: meta("b-token"), Spec: v1alpha1.BindingSpec{Identity: v1alpha1.Identity{TokenSecretRef: ref}}},
			{ObjectMeta: meta("b-none"), Spec: v1alpha1.BindingSpec{Identity: v1alpha1.Identity{UserID: "u"}}},
			{ObjectMeta: meta("conn")}, // a namespace named twice is listed once
		},
	}
	if got, want := set.SecretNamespaces(), []string{"b-token", "be-secret", "conn"}; !slices.Equal(got, want) {
		t.Errorf("SecretNamespaces = %v, want %v", got, want)
	}
}

// PresentSecrets answers for every Secret the set refers to, with every key it needs of it: a connection's
// two tokens, a Binding's token key and a header Secret's key, whatever they are called.
func TestPresentSecrets(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.Headers = []v1alpha1.Header{{Name: "X-Team", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "team", Key: "value"}}}
	set.Bindings[0].Spec.Identity.TokenSecretRef = &v1alpha1.SecretKeyRef{Name: "tok", Key: "oidc"}
	if _, err := Resolve(context.Background(), set, PresentSecrets(set)); err != nil {
		t.Errorf("Resolve with PresentSecrets: %v", err)
	}
	// It is not a stub that reports everything: a reference between objects that does not resolve still fails.
	set = specSet(t)
	set.Bindings[0].Spec.Agent.BackendRef.Name = "missing"
	if _, err := Resolve(context.Background(), set, PresentSecrets(set)); err == nil || !strings.Contains(err.Error(), "AgentBackend") {
		t.Errorf("Resolve with PresentSecrets = %v, want the unresolved backend", err)
	}
}
