package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

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
	byPath    map[string]ResolvedBinding
}

// Binding returns the Binding with the given namespace and name. The result is read-only.
func (s *Snapshot) Binding(namespace, name string) (ResolvedBinding, bool) {
	rb, ok := s.bindings[identity{"Binding", namespace, name}]
	return rb, ok
}

// BindingForChannel returns the Binding that owns a channel of the connection with the given
// namespace and name. The result is read-only.
func (s *Snapshot) BindingForChannel(connNamespace, connName, channel string) (ResolvedBinding, bool) {
	rb, ok := s.byChannel[channelKey{identity{"ChatConnection", connNamespace, connName}, channel}]
	return rb, ok
}

// ChatConnections returns every ChatConnection of the config, sorted by namespace and name:
// those no Binding uses, and those that only name a Binding as their DM default, too. A
// chat adapter is started for each. The slice is the caller's; the connections are read-only.
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

// WebhookPath is the path Alertmanager posts to for the Binding with the given namespace and
// name. Resolve records it in the Binding's status, and the HTTP router serves it.
func WebhookPath(namespace, name string) string {
	return "/hooks/alertmanager/" + namespace + "/" + name
}

// BindingForWebhook returns the Binding whose alert webhook is at path, as in
// "/hooks/alertmanager/sre/sre-alerts". The result is read-only.
func (s *Snapshot) BindingForWebhook(path string) (ResolvedBinding, bool) {
	rb, ok := s.byPath[path]
	return rb, ok
}

// Resolve checks the references between the objects of set and builds a Snapshot. Empty
// namespaces in references mean the referrer's. A reference into another namespace needs the
// target to list the referrer's namespace in allowedNamespaces, except that a ChatConnection
// may name any Binding as its default for direct messages, provided that Binding uses the
// connection. One connection channel belongs to at most one Binding. Through secrets it also
// requires every referenced Secret to exist with the keys it needs, without reading values.
//
// Resolve takes ownership of set: the caller must not use or change it afterwards. It resets
// each Binding's status, which is never input, and sets Status.WebhookPath. On any problem it
// returns every one, joined and grouped by object, and a nil Snapshot. If ctx ends first, the
// error also wraps ctx's, once, ahead of the reference problems found so far; Secret
// problems are then incomplete.
func Resolve(ctx context.Context, set *Set, secrets SecretResolver) (*Snapshot, error) {
	r := &resolver{
		secrets:  secrets,
		backends: index("AgentBackend", set.AgentBackends),
		conns:    index("ChatConnection", set.ChatConnections),
		bindings: index("Binding", set.Bindings),
		snap: &Snapshot{
			bindings:  map[identity]ResolvedBinding{},
			byChannel: map[channelKey]ResolvedBinding{},
			byPath:    map[string]ResolvedBinding{},
		},
	}
	r.snap.conns = r.conns
	for i := range set.Bindings {
		r.binding(&set.Bindings[i])
	}
	for i := range set.ChatConnections {
		r.connection(&set.ChatConnections[i])
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
	backends map[identity]*v1alpha1.AgentBackend
	conns    map[identity]*v1alpha1.ChatConnection
	bindings map[identity]*v1alpha1.Binding
	snap     *Snapshot
	problems []problem
}

// fail records err, if any, as a problem with the field at path of the object from.
func (r *resolver) fail(from identity, path string, err error) {
	if err != nil {
		r.problems = append(r.problems, problem{from, fmt.Errorf("%s: %s: %w", from, path, err)})
	}
}

func (r *resolver) binding(b *v1alpha1.Binding) {
	from := identity{"Binding", b.Namespace, b.Name}
	backend, err := find(r.backends, "AgentBackend", from, b.Spec.Agent.BackendRef,
		func(be *v1alpha1.AgentBackend) []string { return be.Spec.AllowedNamespaces })
	r.fail(from, "spec.agent.backendRef", err)
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
	b.Status = v1alpha1.BindingStatus{}
	if b.Spec.Alerts != nil {
		b.Status.WebhookPath = WebhookPath(b.Namespace, b.Name)
		r.snap.byPath[b.Status.WebhookPath] = rb
	}
}

func (r *resolver) connection(c *v1alpha1.ChatConnection) {
	dm := c.Spec.DirectMessages
	if dm == nil || dm.DefaultBindingRef == nil {
		return
	}
	from := identity{"ChatConnection", c.Namespace, c.Name}
	const path = "spec.directMessages.defaultBindingRef"
	b, err := find(r.bindings, "Binding", from, *dm.DefaultBindingRef, nil)
	r.fail(from, path, err)
	if err != nil {
		return
	}
	ref := b.Spec.Chat.ConnectionRef
	// A Binding whose connection does not exist is reported on the Binding already.
	if used := (identity{"ChatConnection", cmp.Or(ref.Namespace, b.Namespace), ref.Name}); used != from && r.conns[used] != nil {
		r.fail(from, path, fmt.Errorf("%s belongs to %s, not this one", identity{"Binding", b.Namespace, b.Name}, used))
	}
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
// namespace must list from's namespace in the list allowed returns; a nil allowed permits any.
func find[T any](objects map[identity]*T, kind string, from identity, ref v1alpha1.ObjectRef, allowed func(*T) []string) (*T, error) {
	id := identity{kind, cmp.Or(ref.Namespace, from.namespace), ref.Name}
	target := objects[id]
	switch {
	case target == nil:
		return nil, fmt.Errorf("%s not found", id)
	case id.namespace != from.namespace && allowed != nil && !slices.Contains(allowed(target), from.namespace):
		return nil, fmt.Errorf("%s does not allow namespace %s", id, from.namespace)
	}
	return target, nil
}
