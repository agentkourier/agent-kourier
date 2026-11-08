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
// last good one, or zero when there is none and the object is left out. A deleted object that is held on purpose has Deleted
// set and no Generation: the broker's own ChatConnection, which a running broker keeps, and, while that is held, the Bindings
// of it and the AgentBackends they use that went with it. A held Binding that cannot be served (it lost its channel, its
// backend is gone, or its backend's agents does not allow its agent namespace) is counted here and not served, with no
// Rejection: it has no object to put one on.
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
	// AllowedNamespaces is what the refused object's own spec.allowedNamespaces says, read from the raw object when it
	// could not be decoded, for an AgentBackend or ChatConnection. A Binding of another namespace is told only what a valid
	// object would tell it (see resolvePerObject), and this is how it is known which that is.
	AllowedNamespaces []string
	// Message is the problems found, joined, each as "path: message". It is capped, and it names fields and the values
	// that were set in them, which are never Secret values: a Secret is named, not read.
	Message string
}

// ID returns "kind namespace/name".
func (i Invalid) ID() string { return identity{i.Kind, i.Namespace, i.Name}.String() }

// describe says what became of the object in a sentence that a Binding's rejection can carry: the generation and why it is
// refused, and the generation served in its place. withMessage says whether to add the problems found: they are the owner's
// text, and are for a Binding of the object's own namespace only. serving says whether the Binding runs the last good
// generation; where it does not (a refused Binding that resolution then drops), the sentence says that it is not served either.
func (i Invalid) describe(withMessage, serving bool) string {
	var b strings.Builder
	b.WriteString(i.ID())
	switch {
	case i.Deleted:
		b.WriteString(" was deleted")
	default:
		fmt.Fprintf(&b, " generation %d is invalid", i.Generation)
	}
	switch {
	case i.Served > 0 && serving && i.Served == i.Generation:
		// A recreated object starts at generation 1 again, and the one before it was at 1 too.
		fmt.Fprintf(&b, ", serving the last good object, which was at generation %d too", i.Served)
	case i.Served > 0 && serving:
		fmt.Fprintf(&b, ", serving generation %d", i.Served)
	case i.Served > 0:
		fmt.Fprintf(&b, ", and its last good generation %d is not served either", i.Served)
	default:
		b.WriteString(", with no earlier generation to serve")
	}
	if withMessage && i.Message != "" {
		b.WriteString(": " + i.Message)
	}
	return b.String()
}

// rejectionFor says why a Binding in namespace from is left out when the object it names is refused and has no good
// generation, so that ResolvePartial, which does not have it, answers not found. It answers as it would for a valid object
// that is there: another namespace that the object does not allow gets NamespaceNotAllowed, in the very words of a valid
// one, and tells nothing of the object but that it exists, which a valid one tells too; one that it allows, or the object's
// own namespace, gets the rejected reason, with the owner's problems only in the object's own namespace.
func (i Invalid) rejectionFor(from, path string, rejected RejectionReason) (RejectionReason, string) {
	if i.Namespace != from && !slices.Contains(i.AllowedNamespaces, from) {
		return ReasonNamespaceNotAllowed, fmt.Sprintf("%s: %s does not allow namespace %s", path, i.ID(), from)
	}
	return rejected, path + ": " + i.describedTo(from)
}

// describedTo is describe for a Binding in namespace from that runs with the object's generation in force: the owner's
// problems go only to a Binding in the object's own namespace.
func (i Invalid) describedTo(from string) string { return i.describe(i.Namespace == from, true) }

// Report is what a Reloader knows about the config it serves besides the Snapshot: the objects it refused and the
// Bindings that are not served or are served from an older generation. The status writer and the metrics read it. The
// Snapshot is the one the report was made with, so the two are consistent. A Reloader that reads files reports none. A Report is shared by every
// caller: treat it as read-only.
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
// rejected as InvalidSpec. A Binding left out by the agents limit of a backend whose newest generation is refused, or that was
// deleted and is held, has the sentence "(the limit is that of the generation being served: <the object's description>)" added to
// its message: the description says which generation is invalid and which is served, and holds the problems found only when the
// Binding is in the backend's own namespace.
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
			channel:    b.Spec.Chat.Channel,
			backend:    identity{KindAgentBackend, cmp.Or(b.Spec.Agent.BackendRef.Namespace, b.Namespace), b.Spec.Agent.BackendRef.Name},
			conn:       identity{KindChatConnection, cmp.Or(b.Spec.Chat.ConnectionRef.Namespace, b.Namespace), b.Spec.Chat.ConnectionRef.Name},
		}
	}

	snap, partial := ResolvePartial(set)
	var out []Rejection
	said := map[identity]bool{}
	for _, rej := range partial {
		id := identity{KindBinding, rej.Namespace, rej.Name}
		if inv, held := invalid[id]; held && inv.Deleted {
			continue // held after its deletion and not served: it has no object to say anything of, and no generation to count (see Invalid)
		}
		refs := bindings[id]
		rej.Generation = refs.generation
		if rej.Reason == ReasonChannelConflict {
			if w, ok := snap.BindingForChannel(refs.conn.namespace, refs.conn.name, refs.channel); ok {
				if inv, held := invalid[identity{KindBinding, w.Binding.Namespace, w.Binding.Name}]; held && inv.Deleted {
					rej.Message += " (that Binding is deleted, and held until its connection is back)"
				}
			}
		}
		// A Binding at its last good generation that resolution then drops still has a refused spec, which comes first, and
		// the generation that the status says was observed is the new one.
		if inv, ok := invalid[id]; ok {
			rej.Generation = inv.Generation
			if !said[id] {
				said[id] = true
				out = append(out, Rejection{Namespace: id.namespace, Name: id.name, Reason: ReasonInvalidSpec, Message: "spec: " + inv.describe(true, false), Generation: inv.Generation})
			}
		}
		switch rej.Reason {
		case ReasonBackendNotFound:
			if inv, ok := invalid[refs.backend]; ok {
				rej.Reason, rej.Message = inv.rejectionFor(rej.Namespace, "spec.agent.backendRef", ReasonBackendRejected)
			}
		case ReasonAgentNamespaceNotAllowed:
			// The limit is that of the backend's generation in the Set, which is the last good one when a newer one is refused, so
			// the owner who reads the newer one is told which is in force. The generation in force lets the Binding in, so the
			// sentence tells it what flag, below, tells a served Binding of the same backend: the two generations, and the
			// owner's problems only in the backend's own namespace. (rejectionFor is for a backend with no generation in force.)
			if inv, ok := invalid[refs.backend]; ok {
				rej.Message += " (the limit is that of the generation being served: " + inv.describedTo(rej.Namespace) + ")"
			}
		case ReasonConnectionNotFound:
			if inv, ok := invalid[refs.conn]; ok {
				rej.Reason, rej.Message = inv.rejectionFor(rej.Namespace, "spec.chat.connectionRef", ReasonConnectionRejected)
			}
		}
		out = append(out, rej)
	}

	for _, rb := range snap.Bindings() {
		id := identity{KindBinding, rb.Binding.Namespace, rb.Binding.Name}
		if inv, held := invalid[id]; held && inv.Deleted {
			continue // held after its deletion: there is no object to say anything of, and it is served as it was
		}
		flag := func(reason RejectionReason, path string, inv Invalid) {
			out = append(out, Rejection{Namespace: id.namespace, Name: id.name, Reason: reason, Message: path + ": " + inv.describedTo(id.namespace),
				Generation: rb.Binding.Generation, Served: rb.Binding.Generation})
		}
		refs := bindings[id]
		if inv, ok := invalid[id]; ok {
			out = append(out, Rejection{Namespace: id.namespace, Name: id.name, Reason: ReasonInvalidSpec, Message: "spec: " + inv.describe(true, true),
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
			out = append(out, Rejection{Namespace: inv.Namespace, Name: inv.Name, Reason: ReasonInvalidSpec, Message: "spec: " + inv.describe(true, true), Generation: inv.Generation})
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
	channel       string
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
