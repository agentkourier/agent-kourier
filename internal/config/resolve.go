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
// pointers point into the Set that Resolve took over. Treat it as read-only.
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

// Snapshot is the resolved routing table. It is never changed after Resolve returns, so
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

// Bindings returns every Binding of the config, sorted by namespace and name. The slice is the caller's; the Bindings are
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
// target to list the referrer's namespace in allowedNamespaces. One connection channel belongs to at most one Binding. Through secrets it also
// requires every referenced Secret to exist with the keys it needs, without reading values.
//
// Resolve takes ownership of set: the caller must not use or change it afterwards. On any problem it
// returns every one, joined and grouped by object, and a nil Snapshot. A problem with an object of a Set from Load
// starts with the object's location, "<file>#<n> <kind> <namespace>/<name>:", as Load's problems do; an object added or renamed after Load has the location Load gave its new identity, if any, and
// otherwise the problem starts with "<kind> <namespace>/<name>:". If ctx ends first, the
// error also wraps ctx's, once, ahead of the reference problems found so far; Secret
// problems are then incomplete. Each problem wraps ErrInvalid but a Secret that could not be read.
func Resolve(ctx context.Context, set *Set, secrets SecretResolver) (*Snapshot, error) {
	r := &resolver{
		secrets:  secrets,
		sources:  set.sources,
		backends: index("AgentBackend", set.AgentBackends),
		conns:    index("ChatConnection", set.ChatConnections),
		snap: &Snapshot{
			bindings:  map[identity]ResolvedBinding{},
			byChannel: map[channelKey]ResolvedBinding{},
		},
	}
	r.snap.conns = r.conns
	for i := range set.Bindings {
		r.binding(&set.Bindings[i])
	}
	r.checkSecrets(ctx, set)
	// Group each object's problems, keeping the order they were found in.
	slices.SortStableFunc(r.problems, func(a, b problem) int {
		return cmp.Or(cmp.Compare(a.from.kind, b.from.kind), cmp.Compare(a.from.namespace, b.from.namespace), cmp.Compare(a.from.name, b.from.name))
	})
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

// problem is one thing wrong with the object from.
type problem struct {
	from identity
	err  error
}

type resolver struct {
	secrets  SecretResolver
	sources  map[identity]string
	backends map[identity]*v1alpha1.AgentBackend
	conns    map[identity]*v1alpha1.ChatConnection
	snap     *Snapshot
	problems []problem
}

// fail records err, if any, as a problem with the field at path of the object from, starting with where Load found
// the object when it is known. The problem is the config's: it wraps ErrInvalid.
func (r *resolver) fail(from identity, path string, err error) {
	if err == nil {
		return
	}
	r.record(from, path, invalid{err})
}

// record records err as a problem with from at path, unmarked: for one that is not the config's fault.
func (r *resolver) record(from identity, path string, err error) {
	loc := strings.TrimSpace(r.sources[from] + " " + from.String())
	r.problems = append(r.problems, problem{from, fmt.Errorf("%s: %s: %w", loc, path, err)})
}

func (r *resolver) binding(b *v1alpha1.Binding) {
	from := identity{"Binding", b.Namespace, b.Name}
	backend, err := find(r.backends, "AgentBackend", from, b.Spec.Agent.BackendRef,
		func(be *v1alpha1.AgentBackend) []string { return be.Spec.AllowedNamespaces })
	r.fail(from, "spec.agent.backendRef", err)
	if backend != nil && backend.Spec.Dialect != v1alpha1.DialectKagentV1 && backend.Spec.Dialect != v1alpha1.DialectKagentV09 && backend.Spec.Dialect != v1alpha1.DialectA2A {
		r.fail(from, "spec.agent.backendRef", fmt.Errorf("%s uses the %q dialect, supported dialects are %q, %q and %q",
			identity{"AgentBackend", backend.Namespace, backend.Name}, backend.Spec.Dialect, v1alpha1.DialectKagentV1, v1alpha1.DialectKagentV09, v1alpha1.DialectA2A))
		backend = nil
	}
	conn, err := find(r.conns, "ChatConnection", from, b.Spec.Chat.ConnectionRef,
		func(c *v1alpha1.ChatConnection) []string { return c.Spec.AllowedNamespaces })
	r.fail(from, "spec.chat.connectionRef", err)
	if backend == nil || conn == nil {
		return
	}

	rb := ResolvedBinding{b, backend, conn}
	r.snap.bindings[from] = rb
	key := channelKey{identity{"ChatConnection", conn.Namespace, conn.Name}, b.Spec.Chat.Channel}
	if first, taken := r.snap.byChannel[key]; taken {
		r.fail(from, "spec.chat.channel", fmt.Errorf("%s on %s is already bound by %s",
			key.channel, key.connection, identity{"Binding", first.Binding.Namespace, first.Binding.Name}))
		return
	}
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
// namespace must list from's namespace in the list allowed returns.
func find[T any](objects map[identity]*T, kind string, from identity, ref v1alpha1.ObjectRef, allowed func(*T) []string) (*T, error) {
	id := identity{kind, cmp.Or(ref.Namespace, from.namespace), ref.Name}
	target := objects[id]
	switch {
	case target == nil:
		return nil, fmt.Errorf("%s not found", id)
	case id.namespace != from.namespace && !slices.Contains(allowed(target), from.namespace):
		return nil, fmt.Errorf("%s does not allow namespace %s", id, from.namespace)
	}
	return target, nil
}
