package config

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Invalid says that one object of a source that fails one object at a time, the custom resources, did not pass Default
// and Validate, or could not be decoded. File mode has none: a file that does not load fails the whole load.
//
// A broker serves what it can instead: Served is the generation of the same object that the Set holds in its place, the
// last good one, or zero when there is none and the object is left out. A deleted object that is held on purpose (the
// broker's own ChatConnection, which a running broker keeps) has Deleted set and no Generation.
type Invalid struct {
	// Kind is KindAgentBackend, KindChatConnection or KindBinding.
	Kind string
	// Namespace and Name identify the object.
	Namespace, Name string
	// Generation is the metadata.generation of the object that was refused, zero when the object is gone.
	Generation int64
	// Served is the generation the Set holds in its place, or zero when it holds none.
	Served int64
	// Deleted means the object no longer exists and its last good generation is held.
	Deleted bool
	// Message is the problems found, joined, each as "path: message". It is capped, and it names fields and the values
	// that were set in them, which are never Secret values: a Secret is named, not read.
	Message string
}

// ID returns "kind namespace/name".
func (i Invalid) ID() string { return identity{i.Kind, i.Namespace, i.Name}.String() }

// describe says what became of the object in a sentence that a Binding's rejection can carry: the generation and why it is
// refused, and the generation served in its place.
func (i Invalid) describe() string {
	var b strings.Builder
	b.WriteString(i.ID())
	switch {
	case i.Deleted:
		b.WriteString(" was deleted")
	default:
		fmt.Fprintf(&b, " generation %d is invalid", i.Generation)
	}
	if i.Served > 0 {
		fmt.Fprintf(&b, ", serving generation %d", i.Served)
	} else {
		b.WriteString(", with no earlier generation to serve")
	}
	if i.Message != "" {
		b.WriteString(": " + i.Message)
	}
	return b.String()
}

// Report is what a Reloader knows about the config it serves besides the Snapshot: the objects it refused and the
// Bindings that are not served or are served from an older generation. The status writer and the metrics read it. The
// Snapshot is the one the report was made with, so the two are consistent. A Reloader that reads files reports none.
type Report struct {
	// Snapshot is the routing table the other fields describe, nil before the first load.
	Snapshot *Snapshot
	// Rejections is what is wrong with each Binding that is not served as written, sorted by namespace and name. A
	// Binding with Served zero is not in the Snapshot; one with a Served generation is, and runs the older object.
	Rejections []Rejection
	// Invalid lists every object, of any kind, that failed Default or Validate, sorted by kind, namespace and name.
	Invalid []Invalid
}

// Unserved returns the "namespace/name" of each Binding that exists and is not in the Snapshot, sorted. Its sessions
// belong to a Binding that is in the cluster and left out of routing, not to one that was deleted.
func (r Report) Unserved() []string {
	var out []string
	for _, rej := range r.Rejections {
		if rej.Served == 0 {
			out = append(out, rej.Namespace+"/"+rej.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// maxMessage caps the Message of an Invalid, in characters.
const maxMessage = 1024

// CapMessage returns s cut to at most maxMessage characters, with an ellipsis where it was cut. A source uses it for
// Invalid.Message.
func CapMessage(s string) string {
	if utf8.RuneCountInString(s) <= maxMessage {
		return s
	}
	return string([]rune(s)[:maxMessage-1]) + "…"
}

// resolvePerObject resolves set as ResolvePartial does, and adds what a source that fails one object at a time knows: the
// generation of each Binding, the Bindings whose own spec, backend or connection is refused, and the Bindings that are
// left out because an object they name has no good generation (ResolvePartial sees that as not found).
//
// A Binding that is served from the last good generation of a refused object has a Rejection with its Served generation,
// naming the reason: its own spec (InvalidSpec), its backend (BackendRejected) or its connection (ConnectionRejected). A
// Binding that is left out for a reference that is not found, where the target exists but is refused, has the Rejection
// with that reason instead of NotFound. A Binding that is refused and has no last good generation is left out, and is
// rejected as InvalidSpec.
func resolvePerObject(set *Set) (*Snapshot, []Rejection) {
	invalid := make(map[identity]Invalid, len(set.Invalid))
	for _, inv := range set.Invalid {
		invalid[identity{inv.Kind, inv.Namespace, inv.Name}] = inv
	}
	bindings := make(map[identity]*bindingRefs, len(set.Bindings))
	for i := range set.Bindings {
		b := &set.Bindings[i]
		bindings[identity{KindBinding, b.Namespace, b.Name}] = &bindingRefs{
			generation: b.Generation,
			backend:    identity{KindAgentBackend, cmp.Or(b.Spec.Agent.BackendRef.Namespace, b.Namespace), b.Spec.Agent.BackendRef.Name},
			conn:       identity{KindChatConnection, cmp.Or(b.Spec.Chat.ConnectionRef.Namespace, b.Namespace), b.Spec.Chat.ConnectionRef.Name},
		}
	}

	snap, partial := ResolvePartial(set)
	var out []Rejection
	for _, rej := range partial {
		refs := bindings[identity{KindBinding, rej.Namespace, rej.Name}]
		rej.Generation = refs.generation
		switch rej.Reason {
		case ReasonBackendNotFound:
			if inv, ok := invalid[refs.backend]; ok {
				rej.Reason, rej.Message = ReasonBackendRejected, "spec.agent.backendRef: "+inv.describe()
			}
		case ReasonConnectionNotFound:
			if inv, ok := invalid[refs.conn]; ok {
				rej.Reason, rej.Message = ReasonConnectionRejected, "spec.chat.connectionRef: "+inv.describe()
			}
		}
		out = append(out, rej)
	}

	for _, rb := range snap.Bindings() {
		id := identity{KindBinding, rb.Binding.Namespace, rb.Binding.Name}
		flag := func(reason RejectionReason, path string, inv Invalid) {
			out = append(out, Rejection{Namespace: id.namespace, Name: id.name, Reason: reason, Message: path + ": " + inv.describe(),
				Generation: rb.Binding.Generation, Served: rb.Binding.Generation})
		}
		refs := bindings[id]
		if inv, ok := invalid[id]; ok {
			out = append(out, Rejection{Namespace: id.namespace, Name: id.name, Reason: ReasonInvalidSpec, Message: "spec: " + inv.describe(),
				Generation: inv.Generation, Served: inv.Served})
		}
		if inv, ok := invalid[refs.backend]; ok {
			flag(ReasonBackendRejected, "spec.agent.backendRef", inv)
		}
		if inv, ok := invalid[refs.conn]; ok {
			flag(ReasonConnectionRejected, "spec.chat.connectionRef", inv)
		}
	}
	// A refused Binding that has no good generation is not in the Set at all, and so is nowhere above.
	for _, inv := range set.Invalid {
		if inv.Kind == KindBinding && inv.Served == 0 {
			out = append(out, Rejection{Namespace: inv.Namespace, Name: inv.Name, Reason: ReasonInvalidSpec, Message: "spec: " + inv.describe(), Generation: inv.Generation})
		}
	}
	slices.SortStableFunc(out, func(a, b Rejection) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return snap, out
}

// bindingRefs is what resolvePerObject keeps of a Binding of the Set: its generation and the two objects it names.
type bindingRefs struct {
	generation    int64
	backend, conn identity
}

// servedSet returns the objects that snap serves: its Bindings, the backend of each, and its ChatConnections. The Secrets
// that these name are the ones the broker follows, which leaves out the Secrets of an object that was refused or left out,
// and of a backend that no served Binding uses.
func servedSet(snap *Snapshot) *Set {
	s := &Set{}
	seen := map[identity]bool{}
	for _, rb := range snap.Bindings() {
		s.Bindings = append(s.Bindings, *rb.Binding)
		if id := (identity{KindAgentBackend, rb.Backend.Namespace, rb.Backend.Name}); !seen[id] {
			seen[id] = true
			s.AgentBackends = append(s.AgentBackends, *rb.Backend)
		}
	}
	for _, c := range snap.ChatConnections() {
		s.ChatConnections = append(s.ChatConnections, *c)
	}
	return s
}
