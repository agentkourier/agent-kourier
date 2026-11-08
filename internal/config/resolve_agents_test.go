package config

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// notAllowed is the message of a Binding of namespace sre that names an agent in the namespace agentNS on kagent-pilot.
func notAllowed(agentNS string) string {
	return "spec.agent.namespace: AgentBackend agent-kourier-system/kagent-pilot does not allow agents in namespace " + agentNS
}

// TestAgentNamespaces holds both Resolve and ResolvePartial to the AgentBackend's agents field. Both Bindings of spec 7's
// examples are in namespace sre and name an agent in namespace kagent.
func TestAgentNamespaces(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Set)
		wantServed []string
		// rejected names the Bindings that are left out, each with the message of its rejection.
		rejected map[string]string
	}{
		{"no limit: any agent namespace", func(*Set) {}, []string{"sre/chat-only", "sre/sre-alerts"}, nil},
		{"a listed namespace", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"kagent"}}
		}, []string{"sre/chat-only", "sre/sre-alerts"}, nil},
		{"a namespace that is not listed", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"payments"}}
		}, nil, map[string]string{"chat-only": notAllowed("kagent"), "sre-alerts": notAllowed("kagent")}},
		{"sameNamespace does not allow another namespace", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{SameNamespace: true}
		}, nil, map[string]string{"chat-only": notAllowed("kagent"), "sre-alerts": notAllowed("kagent")}},
		{"sameNamespace allows the Binding's own namespace, not the backend's", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{SameNamespace: true}
			sreAlerts(s).Spec.Agent.Namespace = "sre"                                                                                                              // the Binding's
			s.Bindings[slices.IndexFunc(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "chat-only" })].Spec.Agent.Namespace = "agent-kourier-system" // the backend's
		}, []string{"sre/sre-alerts"}, map[string]string{"chat-only": notAllowed("agent-kourier-system")}},
		{"the own namespace without sameNamespace", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"kagent"}}
			sreAlerts(s).Spec.Agent.Namespace = "sre"
		}, []string{"sre/chat-only"}, map[string]string{"sre-alerts": notAllowed("sre")}},
		{"sameNamespace and a list: either", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{SameNamespace: true, Namespaces: []string{"kagent"}}
			sreAlerts(s).Spec.Agent.Namespace = "sre"
		}, []string{"sre/chat-only", "sre/sre-alerts"}, nil},
		{"the limit is per Binding", func(s *Set) {
			s.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"kagent"}}
			sreAlerts(s).Spec.Agent.Namespace = "payments"
		}, []string{"sre/chat-only"}, map[string]string{"sre-alerts": notAllowed("payments")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ResolvePartial", func(t *testing.T) {
				set := specSet(t)
				tc.mutate(set)
				snap, got := ResolvePartial(set)
				var want []Rejection
				for _, name := range slices.Sorted(maps.Keys(tc.rejected)) {
					want = append(want, rejected(name, ReasonAgentNamespaceNotAllowed, tc.rejected[name]))
				}
				if !slices.Equal(got, want) {
					t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
				}
				if s := served(snap); !slices.Equal(s, tc.wantServed) {
					t.Errorf("served = %v; want %v", s, tc.wantServed)
				}
				for _, r := range got {
					if rb, ok := snap.Binding(r.Namespace, r.Name); ok {
						t.Errorf("Binding(%s/%s) = %+v; a rejected Binding must not be served", r.Namespace, r.Name, rb)
					}
				}
			})
			t.Run("Resolve", func(t *testing.T) {
				set := specSet(t)
				tc.mutate(set)
				if len(tc.rejected) == 0 {
					snap, err := Resolve(t.Context(), set, specSecrets())
					if err != nil {
						t.Fatalf("Resolve: %v", err)
					}
					if s := served(snap); !slices.Equal(s, tc.wantServed) {
						t.Errorf("served = %v; want %v", s, tc.wantServed)
					}
					return
				}
				var want []string
				for _, name := range []string{"chat-only", "sre-alerts"} { // Resolve groups a kind's problems by namespace and name
					if msg, ok := tc.rejected[name]; ok {
						at := map[string]string{"chat-only": atChatOnly, "sre-alerts": atSREAlerts}[name]
						want = append(want, at+"Binding sre/"+name+": "+msg)
					}
				}
				wantProblems(t, set, specSecrets(), want...)
			})
		})
	}
}

// A Binding the backend does not allow into its namespace learns nothing of the backend's agents field: the answer is the
// one a backend with no field gives.
func TestAgentNamespacesAreNotToldToABindingTheBackendDoesNotAllow(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.AllowedNamespaces = []string{"payments"}
	set.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"payments"}}
	_, got := ResolvePartial(set)
	if len(got) != 2 {
		t.Fatalf("rejections = %+v; want one for each Binding", got)
	}
	for _, r := range got {
		if r.Reason != ReasonNamespaceNotAllowed || strings.Contains(r.Message, "agents") {
			t.Errorf("rejection = %+v; want NamespaceNotAllowed and no word of the agents field", r)
		}
	}
}

// A Binding that the agents field rejects holds no channel, so that a younger Binding of the same channel is served.
func TestAgentNamespaceRejectedBindingHoldsNoChannel(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"kagent"}}
	sreAlerts(set).CreationTimestamp = at(2)
	set.Bindings = append(set.Bindings, bindingLike(set, "sre", "older", "C0123456789", func(b *v1alpha1.Binding) {
		b.CreationTimestamp = at(1)
		b.Spec.Agent.Namespace = "payments"
	}))
	snap, got := ResolvePartial(set)
	want := []Rejection{rejected("older", ReasonAgentNamespaceNotAllowed, notAllowed("payments"))}
	if !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only", "sre/sre-alerts"}) {
		t.Errorf("served = %v; want chat-only and sre-alerts", s)
	}
}

// A Binding with a rejected agent namespace and a missing connection has both rejections, in the order of its fields.
func TestAgentNamespaceRejectionComesWithTheConnectionsOwn(t *testing.T) {
	set := specSet(t)
	set.AgentBackends[0].Spec.Agents = &v1alpha1.AgentNamespaces{Namespaces: []string{"payments"}}
	sreAlerts(set).Spec.Chat.ConnectionRef.Name = "nope"
	_, got := ResolvePartial(set)
	var mine []Rejection
	for _, r := range got {
		if r.Name == "sre-alerts" {
			mine = append(mine, r)
		}
	}
	want := []Rejection{
		rejected("sre-alerts", ReasonAgentNamespaceNotAllowed, notAllowed("kagent")),
		rejected("sre-alerts", ReasonConnectionNotFound, "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found"),
	}
	if !slices.Equal(mine, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", mine, want)
	}
}
