package config

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

func TestResolveSecrets(t *testing.T) {
	const oidc = "Binding sre/sre-alerts: spec.identity.tokenSecretRef: "
	const webhook = "Binding sre/sre-alerts: spec.alerts.tokenSecretRef: "
	const creds = "ChatConnection courier-system/slack-main: spec.credentialsSecretRef: "
	tests := []struct {
		name   string
		mutate func(*Set, fakeSecrets)
		want   []string
	}{
		{"missing connection Secret", func(_ *Set, f fakeSecrets) { delete(f, "courier-system/slack-main") },
			[]string{creds + "Secret courier-system/slack-main not found"}},
		{"socket mode needs appToken", func(_ *Set, f fakeSecrets) { f["courier-system/slack-main"] = []string{"botToken", "signingSecret"} },
			[]string{creds + `Secret courier-system/slack-main has no key "appToken"`}},
		{"http mode needs signingSecret", func(s *Set, _ fakeSecrets) { s.ChatConnections[0].Spec.Mode = v1alpha1.ModeHTTP },
			[]string{creds + `Secret courier-system/slack-main has no key "signingSecret"`}},
		{"every mode needs botToken", func(_ *Set, f fakeSecrets) { f["courier-system/slack-main"] = []string{"appToken"} },
			[]string{creds + `Secret courier-system/slack-main has no key "botToken"`}},
		{"identity key is the ref's key", func(s *Set, _ fakeSecrets) { sreAlerts(s).Spec.Identity.TokenSecretRef.Key = "jwt" },
			[]string{oidc + `Secret sre/courier-sre-alerts-oidc has no key "jwt"`}},
		{"Secrets are read in the Binding's namespace", func(_ *Set, f fakeSecrets) {
			f["payments/courier-webhook-token"] = f["sre/courier-webhook-token"]
			delete(f, "sre/courier-webhook-token")
		}, []string{webhook + "Secret sre/courier-webhook-token not found"}},
		{"header Secret is read in the backend's namespace", func(s *Set, f fakeSecrets) {
			s.AgentBackends[0].Spec.Headers = []v1alpha1.Header{{Name: "X-Team", ValueSecretRef: &v1alpha1.SecretKeyRef{Name: "team", Key: "value"}}}
			f["courier-system/team"] = []string{"token"}
		}, []string{`AgentBackend courier-system/kagent-pilot: spec.headers[0].valueSecretRef: Secret courier-system/team has no key "value"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, secrets := specSet(t), specSecrets()
			tc.mutate(set, secrets)
			wantProblems(t, set, secrets, tc.want...)
		})
	}

	t.Run("http mode does not need appToken", func(t *testing.T) {
		set, secrets := specSet(t), specSecrets()
		set.ChatConnections[0].Spec.Mode = v1alpha1.ModeHTTP
		secrets["courier-system/slack-main"] = []string{"botToken", "signingSecret"}
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
		"Binding sre/sre-alerts: spec.identity.tokenSecretRef: read Secret sre/courier-sre-alerts-oidc: connection refused",
		"Binding sre/sre-alerts: spec.alerts.tokenSecretRef: read Secret sre/courier-webhook-token: connection refused",
		"ChatConnection courier-system/slack-main: spec.credentialsSecretRef: read Secret courier-system/slack-main: connection refused",
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
	if !errors.Is(err, context.Canceled) || len(got) != 2 || !strings.HasSuffix(got[1].Error(), "AgentBackend courier-system/nope not found") || secrets.reads != 1 {
		t.Errorf("Resolve error = %v after %d reads; want context.Canceled once, then the reference problem, after 1 read", err, secrets.reads)
	}
}
