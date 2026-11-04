package config

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// served returns "namespace/name" of each Binding of snap, in the order Bindings gives them.
func served(snap *Snapshot) []string {
	var out []string
	for _, rb := range snap.Bindings() {
		out = append(out, rb.Binding.Namespace+"/"+rb.Binding.Name)
	}
	return out
}

// rejected is a Rejection of the Binding sre/name.
func rejected(name string, reason RejectionReason, message string) Rejection {
	return Rejection{Namespace: "sre", Name: name, Reason: reason, Message: message}
}

// TestResolvePartialRejections has a case for each reason a Binding is left out of the snapshot. The Bindings that are
// not rejected stay in it.
func TestResolvePartialRejections(t *testing.T) {
	const (
		dialect = `spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot uses the "unknown" dialect, supported dialects are "kagent-v1" and "a2a"`
		noAllow = " does not allow namespace sre"
	)
	tests := []struct {
		name       string
		mutate     func(*Set)
		want       []Rejection
		wantServed []string
	}{
		{"BackendNotFound", func(s *Set) { sreAlerts(s).Spec.Agent.BackendRef.Name = "nope" },
			[]Rejection{rejected("sre-alerts", ReasonBackendNotFound, "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found")},
			[]string{"sre/chat-only"}},
		{"ConnectionNotFound", func(s *Set) { sreAlerts(s).Spec.Chat.ConnectionRef.Name = "nope" },
			[]Rejection{rejected("sre-alerts", ReasonConnectionNotFound, "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found")},
			[]string{"sre/chat-only"}},
		{"NamespaceNotAllowed by the backend", func(s *Set) { s.AgentBackends[0].Spec.AllowedNamespaces = []string{"payments"} },
			[]Rejection{
				rejected("chat-only", ReasonNamespaceNotAllowed, "spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot"+noAllow),
				rejected("sre-alerts", ReasonNamespaceNotAllowed, "spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot"+noAllow),
			},
			nil},
		{"NamespaceNotAllowed by the connection", func(s *Set) { s.ChatConnections[0].Spec.AllowedNamespaces = nil },
			[]Rejection{
				rejected("chat-only", ReasonNamespaceNotAllowed, "spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main"+noAllow),
				rejected("sre-alerts", ReasonNamespaceNotAllowed, "spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main"+noAllow),
			},
			nil},
		{"BackendRejected: a dialect that is not built", func(s *Set) { s.AgentBackends[0].Spec.Dialect = "unknown" },
			[]Rejection{rejected("chat-only", ReasonBackendRejected, dialect), rejected("sre-alerts", ReasonBackendRejected, dialect)},
			nil},
		{"ChannelConflict: the older Binding wins", func(s *Set) {
			sreAlerts(s).CreationTimestamp = at(2)
			s.Bindings = append(s.Bindings, bindingLike(s, "sre", "older", "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(1) }))
		},
			[]Rejection{rejected("sre-alerts", ReasonChannelConflict, "spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/older")},
			[]string{"sre/chat-only", "sre/older"}},
		{"ChannelConflict: an older Binding with a broken reference holds no channel", func(s *Set) {
			sreAlerts(s).CreationTimestamp = at(2)
			s.Bindings = append(s.Bindings, bindingLike(s, "sre", "older-broken", "C0123456789", func(b *v1alpha1.Binding) {
				b.CreationTimestamp = at(1)
				b.Spec.Agent.BackendRef.Name = "nope"
			}))
		},
			[]Rejection{rejected("older-broken", ReasonBackendNotFound, "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found")},
			[]string{"sre/chat-only", "sre/sre-alerts"}},
		{"both references fail", func(s *Set) {
			sreAlerts(s).Spec.Agent.BackendRef.Name = "nope"
			sreAlerts(s).Spec.Chat.ConnectionRef.Name = "nope"
		}, []Rejection{
			rejected("sre-alerts", ReasonBackendNotFound, "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"),
			rejected("sre-alerts", ReasonConnectionNotFound, "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found"),
		}, []string{"sre/chat-only"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := specSet(t)
			tc.mutate(set)
			snap, got := ResolvePartial(set)
			if snap == nil {
				t.Fatal("ResolvePartial returned a nil Snapshot")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, tc.want)
			}
			if s := served(snap); !slices.Equal(s, tc.wantServed) {
				t.Errorf("served = %v; want %v", s, tc.wantServed)
			}
			// A Binding that was left out is in no lookup, whichever way it is asked for.
			for _, r := range got {
				if rb, ok := snap.Binding(r.Namespace, r.Name); ok {
					t.Errorf("Binding(%s/%s) = %+v; a rejected Binding must not be served", r.Namespace, r.Name, rb)
				}
			}
		})
	}
}

// TestResolvePartialChannelConflict: the oldest creationTimestamp wins a channel, then namespace and name. A Binding
// without a timestamp, as every one read from a file is, counts as the oldest. Every loser names the one winner, and the
// loser is served by no lookup, the channel's included.
func TestResolvePartialChannelConflict(t *testing.T) {
	tests := []struct {
		name    string
		stamps  map[string]int // minutes after the base instant; absent means no timestamp
		winner  string
		losers  []string
		inOrder []string // the order the Bindings are given in
	}{
		{"oldest wins whatever the order", map[string]int{"a": 3, "b": 1, "c": 2}, "b", []string{"a", "c"}, []string{"a", "b", "c"}},
		{"oldest wins when it is last", map[string]int{"a": 1, "b": 2, "c": 3}, "a", []string{"b", "c"}, []string{"c", "b", "a"}},
		{"equal timestamps fall to name", map[string]int{"a": 1, "b": 1, "c": 1}, "a", []string{"b", "c"}, []string{"c", "b", "a"}},
		{"no timestamps fall to name", nil, "a", []string{"b", "c"}, []string{"b", "c", "a"}},
		{"no timestamp is older than any", map[string]int{"a": 1, "b": 2}, "c", []string{"a", "b"}, []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := specSet(t)
			set.Bindings = nil
			base := specSet(t)
			for _, n := range tc.inOrder {
				set.Bindings = append(set.Bindings, bindingLike(base, "sre", n, "C0123456789", func(b *v1alpha1.Binding) {
					if m, ok := tc.stamps[n]; ok {
						b.CreationTimestamp = at(m)
					}
				}))
			}
			snap, got := ResolvePartial(set)
			var want []Rejection
			for _, l := range tc.losers {
				want = append(want, rejected(l, ReasonChannelConflict,
					"spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/"+tc.winner))
			}
			if !slices.Equal(got, want) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
			}
			if rb, ok := snap.BindingForChannel("agent-kourier-system", "slack-main", "C0123456789"); !ok || rb.Binding.Name != tc.winner {
				t.Errorf("BindingForChannel = %+v, %v; want %s", rb, ok, tc.winner)
			}
			if s := served(snap); !slices.Equal(s, []string{"sre/" + tc.winner}) {
				t.Errorf("served = %v; want only the winner", s)
			}
		})
	}
}

// TestResolvePartialChannelTieBreakIsNamespaceThenName: at equal timestamps the smaller namespace wins even when its
// name is the larger, in either order of the set.
func TestResolvePartialChannelTieBreakIsNamespaceThenName(t *testing.T) {
	for _, order := range [][2]string{{"aaa/zed", "zzz/abc"}, {"zzz/abc", "aaa/zed"}} {
		t.Run(strings.Join(order[:], " then "), func(t *testing.T) {
			set := specSet(t)
			set.Bindings = nil
			set.AgentBackends[0].Spec.AllowedNamespaces = []string{"aaa", "zzz"}
			set.ChatConnections[0].Spec.AllowedNamespaces = []string{"aaa", "zzz"}
			base := specSet(t)
			for _, id := range order {
				ns, name, _ := strings.Cut(id, "/")
				set.Bindings = append(set.Bindings, bindingLike(base, ns, name, "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(1) }))
			}
			snap, got := ResolvePartial(set)
			want := []Rejection{{Namespace: "zzz", Name: "abc", Reason: ReasonChannelConflict,
				Message: "spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding aaa/zed"}}
			if !slices.Equal(got, want) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
			}
			if s := served(snap); !slices.Equal(s, []string{"aaa/zed"}) {
				t.Errorf("served = %v; want aaa/zed", s)
			}
		})
	}
}

// TestResolveStrictChannelWinnerIsFirstInSet: the file route is unchanged, and the first Binding in the set wins whatever
// the creation timestamps are.
func TestResolveStrictChannelWinnerIsFirstInSet(t *testing.T) {
	set := specSet(t)
	sreAlerts(set).CreationTimestamp = at(9)
	set.Bindings = append(set.Bindings, bindingLike(set, "sre", "older", "C0123456789", func(b *v1alpha1.Binding) { b.CreationTimestamp = at(1) }))
	wantProblems(t, set, specSecrets(), "Binding sre/older: spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/sre-alerts")
}

// A Secret that is missing, or lacks a key, does not take a Binding out of the snapshot: the prober reports it, and
// leaving the snapshot costs the Binding its intake, its queued replies and its sessions' recovery. Resolve still
// refuses the same set.
func TestResolvePartialKeepsABindingWhoseSecretIsMissing(t *testing.T) {
	set := specSet(t)
	if _, err := Resolve(t.Context(), specSet(t), fakeSecrets{}); err == nil {
		t.Fatal("Resolve accepted a config with no Secrets")
	}
	snap, got := ResolvePartial(set)
	if len(got) != 0 {
		t.Errorf("rejections = %+v; want none", got)
	}
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only", "sre/sre-alerts"}) {
		t.Errorf("served = %v; want both Bindings", s)
	}
	if _, ok := snap.BindingForChannel("agent-kourier-system", "slack-main", "C0123456789"); !ok {
		t.Error("the channel of a Binding with a missing Secret is not routed")
	}
}

// TestResolvePartialMessySet pins the whole outcome for a set with every kind of problem, and what it leaves served.
func TestResolvePartialMessySet(t *testing.T) {
	snap, got := ResolvePartial(messySet(t))
	const (
		allow = " does not allow namespace other"
		bound = "spec.chat.channel: C0123456789 on ChatConnection agent-kourier-system/slack-main is already bound by Binding sre/zz-early"
	)
	other := func(reason RejectionReason, message string) Rejection {
		return Rejection{Namespace: "other", Name: "outsider", Reason: reason, Message: message}
	}
	want := []Rejection{
		other(ReasonNamespaceNotAllowed, "spec.agent.backendRef: AgentBackend agent-kourier-system/kagent-pilot"+allow),
		other(ReasonNamespaceNotAllowed, "spec.chat.connectionRef: ChatConnection agent-kourier-system/slack-main"+allow),
		rejected("aa-late", ReasonChannelConflict, bound),
		rejected("no-backend", ReasonBackendNotFound, "spec.agent.backendRef: AgentBackend agent-kourier-system/nope not found"),
		rejected("no-conn", ReasonConnectionNotFound, "spec.chat.connectionRef: ChatConnection agent-kourier-system/nope not found"),
		rejected("odd-dialect", ReasonBackendRejected, `spec.agent.backendRef: AgentBackend agent-kourier-system/weird uses the "unknown" dialect, supported dialects are "kagent-v1" and "a2a"`),
		rejected("sre-alerts", ReasonChannelConflict, bound),
	}
	if !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only", "sre/zz-early"}) {
		t.Errorf("served = %v; want chat-only and zz-early", s)
	}
}

// outcome renders what ResolvePartial returned, one line each, so that two results can be compared.
func outcome(snap *Snapshot, rejections []Rejection) string {
	var b strings.Builder
	for _, rb := range snap.Bindings() {
		fmt.Fprintf(&b, "served %s/%s on %s/%s with %s/%s\n", rb.Binding.Namespace, rb.Binding.Name,
			rb.Connection.Namespace, rb.Connection.Name, rb.Backend.Namespace, rb.Backend.Name)
		if won, ok := snap.BindingForChannel(rb.Connection.Namespace, rb.Connection.Name, rb.Binding.Spec.Chat.Channel); ok {
			fmt.Fprintf(&b, "  channel %s is %s/%s's\n", rb.Binding.Spec.Chat.Channel, won.Binding.Namespace, won.Binding.Name)
		}
	}
	for _, c := range snap.ChatConnections() {
		fmt.Fprintf(&b, "connection %s/%s\n", c.Namespace, c.Name)
	}
	for _, r := range rejections {
		fmt.Fprintf(&b, "%+v\n", r)
	}
	return b.String()
}

// TestResolvePartialIgnoresTheOrderOfObjects: the snapshot and the rejections are the same whatever order the objects
// came in, which is the order an informer cache lists them in. The winner of a channel conflict above all must not move.
func TestResolvePartialIgnoresTheOrderOfObjects(t *testing.T) {
	want := outcome(ResolvePartial(messySet(t)))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 100 {
		set := messySet(t)
		rng.Shuffle(len(set.Bindings), func(a, b int) { set.Bindings[a], set.Bindings[b] = set.Bindings[b], set.Bindings[a] })
		rng.Shuffle(len(set.AgentBackends), func(a, b int) {
			set.AgentBackends[a], set.AgentBackends[b] = set.AgentBackends[b], set.AgentBackends[a]
		})
		rng.Shuffle(len(set.ChatConnections), func(a, b int) {
			set.ChatConnections[a], set.ChatConnections[b] = set.ChatConnections[b], set.ChatConnections[a]
		})
		if got := outcome(ResolvePartial(set)); got != want {
			t.Fatalf("shuffle %d changed the outcome:\n%s\nwant:\n%s", i, got, want)
		}
	}
}

// An empty set resolves to an empty snapshot and nothing is rejected: the caller never has to check for nil.
func TestResolvePartialEmptySet(t *testing.T) {
	snap, got := ResolvePartial(&Set{})
	if snap == nil || len(snap.Bindings()) != 0 || len(snap.ChatConnections()) != 0 || len(got) != 0 {
		t.Errorf("ResolvePartial(empty) = %+v, %+v; want an empty Snapshot and no rejections", snap, got)
	}
}
