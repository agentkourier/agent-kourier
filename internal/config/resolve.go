package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// ResolvedBinding is a Binding with the backend and connection its references name. Its
// pointers point into the Set that Resolve or ResolvePartial took over. Treat it as read-only.
type ResolvedBinding struct {
	Binding    *v1alpha1.Binding
	Backend    *v1alpha1.AgentBackend
	Connection *v1alpha1.ChatConnection
}

// channelKey identifies a chat channel on one connection.
type channelKey struct {
	connection identity
	channel    string
}

// Snapshot is the resolved routing table. It is never changed after Resolve or ResolvePartial returns, so
// any number of goroutines may use it at once. Everything it returns is shared with it and
// with other callers, and callers must treat it as read-only, as with client-go's listers.
type Snapshot struct {
	conns     map[identity]*v1alpha1.ChatConnection
	bindings  map[identity]ResolvedBinding
	byChannel map[channelKey]ResolvedBinding
}

// Binding returns the Binding with the given namespace and name. The result is read-only.
func (s *Snapshot) Binding(namespace, name string) (ResolvedBinding, bool) {
	rb, ok := s.bindings[identity{"Binding", namespace, name}]
	return rb, ok
}

// Bindings returns every Binding the Snapshot serves, sorted by namespace and name. The slice is the caller's; the Bindings are
// read-only.
func (s *Snapshot) Bindings() []ResolvedBinding {
	out := make([]ResolvedBinding, 0, len(s.bindings))
	for _, rb := range s.bindings {
		out = append(out, rb)
	}
	slices.SortFunc(out, func(a, b ResolvedBinding) int {
		return cmp.Or(cmp.Compare(a.Binding.Namespace, b.Binding.Namespace), cmp.Compare(a.Binding.Name, b.Binding.Name))
	})
	return out
}

// BindingForChannel returns the Binding that owns a channel of the connection with the given
// namespace and name. The result is read-only.
func (s *Snapshot) BindingForChannel(connNamespace, connName, channel string) (ResolvedBinding, bool) {
	rb, ok := s.byChannel[channelKey{identity{"ChatConnection", connNamespace, connName}, channel}]
	return rb, ok
}

// ChatConnections returns every ChatConnection of the config, sorted by namespace and name,
// those no Binding uses too. A chat adapter is started for each. The slice is the caller's; the connections are read-only.
func (s *Snapshot) ChatConnections() []*v1alpha1.ChatConnection {
	out := make([]*v1alpha1.ChatConnection, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *v1alpha1.ChatConnection) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// Resolve checks the references between the objects of set and builds a Snapshot. Empty
// namespaces in references mean the referrer's. A reference into another namespace needs the
// target to list the referrer's namespace in allowedNamespaces. One connection channel belongs to at most one Binding,
// the first of them in set. Through secrets it also
// requires every referenced Secret to exist with the keys it needs, without reading values.
//
// Resolve takes ownership of set: the caller must not use or change it afterwards. On any problem it
// returns every one, joined and grouped by object, and a nil Snapshot. A problem with an object of a Set from Load
// starts with the object's location, "<file>#<n> <kind> <namespace>/<name>:", as Load's problems do; an object added or renamed after Load has the location Load gave its new identity, if any, and
// otherwise the problem starts with "<kind> <namespace>/<name>:". If ctx ends first, the
// error also wraps ctx's, once, ahead of the reference problems found so far; Secret
// problems are then incomplete. Each problem wraps ErrInvalid but a Secret that could not be read.
//
// It is all or nothing, as the config files are, and the first Binding of set to claim a channel keeps it. ResolvePartial
// serves what it can, and decides a channel by age.
func Resolve(ctx context.Context, set *Set, secrets SecretResolver) (*Snapshot, error) {
	r := newResolver(set)
	r.secrets = secrets
	r.resolveReferences(set)
	r.checkSecrets(ctx, set)
	r.groupProblems()
	var errs []error
	if err := ctx.Err(); err != nil {
		errs = append(errs, fmt.Errorf("resolve config: %w", err))
	}
	for _, p := range r.problems {
		errs = append(errs, p.err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return r.snap, nil
}

// RejectionReason says why a Binding is not in the Snapshot that ResolvePartial returns. It is one of the Reason
// constants.
type RejectionReason string

// The reasons ResolvePartial rejects a Binding. Each is one rule of Resolve. The names are the ones a Binding's Accepted
// condition is to carry.
const (
	// ReasonBackendNotFound: the backendRef names no AgentBackend.
	ReasonBackendNotFound RejectionReason = "BackendNotFound"
	// ReasonConnectionNotFound: the connectionRef names no ChatConnection.
	ReasonConnectionNotFound RejectionReason = "ConnectionNotFound"
	// ReasonNamespaceNotAllowed: the AgentBackend or ChatConnection is in another namespace and does not list the
	// Binding's in allowedNamespaces.
	ReasonNamespaceNotAllowed RejectionReason = "NamespaceNotAllowed"
	// ReasonBackendRejected: the AgentBackend is itself invalid. Today that is a dialect the broker has no adapter for.
	ReasonBackendRejected RejectionReason = "BackendRejected"
	// ReasonChannelConflict: another Binding owns the channel on the connection.
	ReasonChannelConflict RejectionReason = "ChannelConflict"
	// ReasonInvalidSpec: the Binding does not pass Default and Validate. ResolvePartial never gives it, since a Set holds
	// only objects that passed; a source that fails one object at a time does (see Report).
	ReasonInvalidSpec RejectionReason = "InvalidSpec"
	// ReasonConnectionRejected: the ChatConnection is itself invalid, and has no good generation to serve in its place or
	// is served from an older one. Like ReasonInvalidSpec it comes from a source that fails one object at a time, where
	// ResolvePartial sees an object left out and says it is not found.
	ReasonConnectionRejected RejectionReason = "ConnectionRejected"
)

// Rejection says why one Binding is not in the Snapshot that ResolvePartial returned. A Binding has one for each of its
// references that fails, and one if it loses its channel. A Rejection holds no Secret value.
type Rejection struct {
	// Namespace and Name identify the rejected Binding.
	Namespace, Name string
	// Reason is the rule that rejected the object.
	Reason RejectionReason
	// Message names the field and what is wrong with it, as a problem that Resolve returns does after the object's
	// location, for example "spec.chat.channel: C0123456789 on ChatConnection ns/conn is already bound by Binding ns/other".
	Message string
	// Generation is the metadata.generation of the rejected Binding, and Served the generation of it that the Snapshot
	// serves, zero when it is left out. ResolvePartial leaves both zero, since it knows no generation: a Reloader of a source
	// that fails one object at a time fills them in (Report). A Rejection with Served set means the Binding runs, from an
	// older generation of itself or with an older generation of an object it names, and says why that is not the whole truth.
	Generation, Served int64
}

// ResolvePartial checks the references between the objects of set as Resolve does, and builds a Snapshot of what is
// sound. A Binding whose backendRef or connectionRef cannot be followed, whose backend is on a dialect that is not
// built, or that lost its channel to another Binding is left out of the Snapshot and has a Rejection. The Rejections are
// sorted by Namespace and Name, and a Binding's own are in the order of its fields.
//
// Where two Bindings claim a channel of a connection the oldest creationTimestamp wins, then the smaller namespace and
// name; a Binding without a timestamp, as one read from a file has none, counts as the oldest. Only a Binding whose
// references resolve claims a channel. Each loser is rejected, naming the winner. Which Binding owns a channel does not
// depend on the order of set.
//
// Age alone decides, so "a later Binding cannot take a channel" is about creation age, not about who was served first:
// an older Binding edited onto a younger one's channel displaces it, and so does an older Binding whose broken reference
// heals.
//
// A Secret that is missing, or lacks a key, is not a rejection: the Binding stays in the Snapshot and the prober reports
// the Secret. ResolvePartial reads no Secret and does no I/O. Like Resolve it takes ownership of set, and it reorders
// set.Bindings by age. The Snapshot is never nil.
func ResolvePartial(set *Set) (*Snapshot, []Rejection) {
	// The first Binding to claim a channel keeps it, so the oldest goes first.
	slices.SortStableFunc(set.Bindings, func(a, b v1alpha1.Binding) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	r := newResolver(set)
	r.resolveReferences(set)
	r.groupProblems()
	var rejections []Rejection
	for _, p := range r.problems {
		if p.rejection != nil {
			rejections = append(rejections, *p.rejection)
		}
	}
	return r.snap, rejections
}

// problem is one thing wrong with the object from. One that leaves a Binding out of a partial Snapshot has its
// rejection too.
type problem struct {
	from      identity
	err       error
	rejection *Rejection
}

type resolver struct {
	secrets  SecretResolver
	sources  map[identity]string
	backends map[identity]*v1alpha1.AgentBackend
	conns    map[identity]*v1alpha1.ChatConnection
	snap     *Snapshot
	problems []problem
}

// newResolver returns a resolver for set. Resolve adds the Secret reader.
func newResolver(set *Set) *resolver {
	r := &resolver{
		sources:  set.sources,
		backends: index("AgentBackend", set.AgentBackends),
		conns:    index("ChatConnection", set.ChatConnections),
		snap: &Snapshot{
			bindings:  map[identity]ResolvedBinding{},
			byChannel: map[channelKey]ResolvedBinding{},
		},
	}
	r.snap.conns = r.conns
	return r
}

// resolveReferences follows the references of each Binding of set, in order. The first to claim a channel keeps it.
func (r *resolver) resolveReferences(set *Set) {
	for i := range set.Bindings {
		r.binding(&set.Bindings[i])
	}
}

// groupProblems groups each object's problems, keeping the order they were found in.
func (r *resolver) groupProblems() {
	slices.SortStableFunc(r.problems, func(a, b problem) int {
		return cmp.Or(cmp.Compare(a.from.kind, b.from.kind), cmp.Compare(a.from.namespace, b.from.namespace), cmp.Compare(a.from.name, b.from.name))
	})
}

// fail records err, if any, as a problem with the field at path of the object from, starting with where Load found
// the object when it is known. The problem is the config's: it wraps ErrInvalid.
func (r *resolver) fail(from identity, path string, err error) {
	if err == nil {
		return
	}
	r.record(from, path, invalid{err})
}

// reject is fail for a problem that leaves the Binding from out of a partial Snapshot, for the reason given.
func (r *resolver) reject(from identity, reason RejectionReason, path string, err error) {
	if err == nil {
		return
	}
	r.fail(from, path, err)
	r.problems[len(r.problems)-1].rejection = &Rejection{
		Namespace: from.namespace, Name: from.name, Reason: reason, Message: path + ": " + err.Error(),
	}
}

// record records err as a problem with from at path, unmarked: for one that is not the config's fault.
func (r *resolver) record(from identity, path string, err error) {
	loc := strings.TrimSpace(r.sources[from] + " " + from.String())
	r.problems = append(r.problems, problem{from: from, err: fmt.Errorf("%s: %s: %w", loc, path, err)})
}

// binding follows the references of b and rejects it for each one that fails, and for a channel that an earlier Binding
// has claimed. A Binding that is rejected is not in the Snapshot.
func (r *resolver) binding(b *v1alpha1.Binding) {
	from := identity{"Binding", b.Namespace, b.Name}
	backend, reason, err := find(r.backends, "AgentBackend", ReasonBackendNotFound, from, b.Spec.Agent.BackendRef,
		func(be *v1alpha1.AgentBackend) []string { return be.Spec.AllowedNamespaces })
	r.reject(from, reason, "spec.agent.backendRef", err)
	if backend != nil && backend.Spec.Dialect != v1alpha1.DialectKagentV1 && backend.Spec.Dialect != v1alpha1.DialectA2A {
		r.reject(from, ReasonBackendRejected, "spec.agent.backendRef", fmt.Errorf("%s uses the %q dialect, supported dialects are %q and %q",
			identity{"AgentBackend", backend.Namespace, backend.Name}, backend.Spec.Dialect, v1alpha1.DialectKagentV1, v1alpha1.DialectA2A))
		backend = nil
	}
	conn, reason, err := find(r.conns, "ChatConnection", ReasonConnectionNotFound, from, b.Spec.Chat.ConnectionRef,
		func(c *v1alpha1.ChatConnection) []string { return c.Spec.AllowedNamespaces })
	r.reject(from, reason, "spec.chat.connectionRef", err)
	if backend == nil || conn == nil {
		return
	}

	rb := ResolvedBinding{b, backend, conn}
	key := channelKey{identity{"ChatConnection", conn.Namespace, conn.Name}, b.Spec.Chat.Channel}
	if first, taken := r.snap.byChannel[key]; taken {
		r.reject(from, ReasonChannelConflict, "spec.chat.channel", fmt.Errorf("%s on %s is already bound by %s",
			key.channel, key.connection, identity{"Binding", first.Binding.Namespace, first.Binding.Name}))
		return
	}
	r.snap.bindings[from] = rb
	r.snap.byChannel[key] = rb
}

// index maps the identity of each object of one kind to the object.
func index[T any, PT interface {
	*T
	GetName() string
	GetNamespace() string
}](kind string, items []T) map[identity]PT {
	m := make(map[identity]PT, len(items))
	for i := range items {
		p := PT(&items[i])
		m[identity{kind, p.GetNamespace(), p.GetName()}] = p
	}
	return m
}

// find looks up the object ref names, as seen from the object from. A target in another
// namespace must list from's namespace in the list allowed returns. A failure comes with its reason: notFound if there
// is no such object.
func find[T any](objects map[identity]*T, kind string, notFound RejectionReason, from identity, ref v1alpha1.ObjectRef, allowed func(*T) []string) (*T, RejectionReason, error) {
	id := identity{kind, cmp.Or(ref.Namespace, from.namespace), ref.Name}
	target := objects[id]
	switch {
	case target == nil:
		return nil, notFound, fmt.Errorf("%s not found", id)
	case id.namespace != from.namespace && !slices.Contains(allowed(target), from.namespace):
		return nil, ReasonNamespaceNotAllowed, fmt.Errorf("%s does not allow namespace %s", id, from.namespace)
	}
	return target, "", nil
}
