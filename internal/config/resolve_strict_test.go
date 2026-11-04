package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// at returns a creation timestamp that many minutes after a fixed instant.
func at(minutes int) metav1.Time {
	return metav1.NewTime(time.Date(2026, 11, 4, 12, minutes, 0, 0, time.UTC))
}

// bindingLike returns a copy of the sre-alerts Binding with its own name, namespace and channel, which mutate may
// change further. The copy has no source location, so its problems have none.
func bindingLike(s *Set, namespace, name, channel string, mutate func(*v1alpha1.Binding)) v1alpha1.Binding {
	b := *sreAlerts(s)
	b.Namespace, b.Name, b.Spec.Chat.Channel = namespace, name, channel
	if mutate != nil {
		mutate(&b)
	}
	return b
}

// messySet is spec 7's example with one Binding for each way a reference can fail, a backend on a dialect that is not
// built, and three Bindings on one channel. sre-alerts is the first of those three in the set and the second oldest.
func messySet(t *testing.T) *Set {
	t.Helper()
	set := specSet(t)
	weird := set.AgentBackends[0]
	weird.Name, weird.Spec.Dialect = "weird", "unknown"
	set.AgentBackends = append(set.AgentBackends, weird)
	sreAlerts(set).CreationTimestamp = at(2)
	set.Bindings = append(set.Bindings,
		bindingLike(set, "sre", "no-backend", "C1", func(b *v1alpha1.Binding) { b.Spec.Agent.BackendRef.Name = "nope" }),
		bindingLike(set, "sre", "no-conn", "C2", func(b *v1alpha1.Binding) { b.Spec.Chat.ConnectionRef.Name = "nope" }),
		bindingLike(set, "other", "outsider", "C3", nil), // refers into agent-kourier-system, which does not list "other"
		bindingLike(set, "sre", "odd-dialect", "C4", func(b *v1alpha1.Binding) { b.Spec.Agent.BackendRef.Name = "weird" }),
		bindingLike(set, "sre", "aa-late", "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(3) }),
		bindingLike(set, "sre", "zz-early", "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(1) }),
	)
	return set
}

// The text Resolve returns for messySet with none of its Secrets present, one problem to a line. It is what Resolve said
// before it was split into a snapshot and rejections, and it must not change: the file route stays all or nothing, with
// the same words, the same order, and the first Binding in the set winning a channel, whatever the creation timestamps say.
const wantMessyStrict = `Binding other/outsider: spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot does not allow namespace other
Binding other/outsider: spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main does not allow namespace other
Binding other/outsider: spec.identity.tokenSecretRef: Secret other/agent-kourier-sre-alerts-oidc not found
Binding sre/aa-late: spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/sre-alerts
Binding sre/aa-late: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
Binding sre/no-backend: spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found
Binding sre/no-backend: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
Binding sre/no-conn: spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found
Binding sre/no-conn: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
Binding sre/odd-dialect: spec.agent.backendRef: AgentBackend agent-kourier-system/weird uses the "unknown" dialect, supported dialects are "kagent-v1" and "a2a"
Binding sre/odd-dialect: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
binding.yaml#1 Binding sre/sre-alerts: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
Binding sre/zz-early: spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/sre-alerts
Binding sre/zz-early: spec.identity.tokenSecretRef: Secret sre/agent-kourier-sre-alerts-oidc not found
chatconnection.yaml#1 ChatConnection agent-kourier-system/slack-main: spec.credentialsSecretRef: Secret agent-kourier-system/slack-main not found`

func TestResolveStrictTextIsPinned(t *testing.T) {
	snap, err := Resolve(context.Background(), messySet(t), fakeSecrets{})
	if snap != nil || err == nil {
		t.Fatalf("Resolve = %+v, %v; want a nil Snapshot and an error", snap, err)
	}
	if got := err.Error(); got != wantMessyStrict {
		t.Errorf("Resolve error:\n%s\nwant:\n%s", got, wantMessyStrict)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Error("Resolve error does not wrap ErrInvalid")
	}
	for _, e := range flatten(err) {
		if !errors.Is(e, ErrInvalid) {
			t.Errorf("problem %q does not wrap ErrInvalid", e)
		}
	}
}

// A Secret that cannot be read is not the config's fault, so it does not wrap ErrInvalid, and Resolve says so for
// that problem alone.
func TestResolveStrictKeepsReadErrorsApartFromInvalid(t *testing.T) {
	set := messySet(t)
	_, err := Resolve(context.Background(), set, failingSecrets{})
	var invalidCount, otherCount int
	for _, e := range flatten(err) {
		if errors.Is(e, ErrInvalid) {
			invalidCount++
			continue
		}
		otherCount++
		if !strings.Contains(e.Error(), "connection refused") {
			t.Errorf("problem %q is not ErrInvalid and not a read error", e)
		}
	}
	if invalidCount == 0 || otherCount == 0 {
		t.Errorf("got %d invalid and %d other problems; want some of each", invalidCount, otherCount)
	}
}
