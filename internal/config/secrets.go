package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// ErrSecretNotFound is what a SecretResolver wraps when the Secret does not exist.
var ErrSecretNotFound = errors.New("secret not found")

// SecretResolver reports which keys a Secret has. It must never return values: not in its
// return values, and not in its errors, which Resolve shows to operators.
type SecretResolver interface {
	// Keys returns the key names of the Secret, or an error wrapping ErrSecretNotFound.
	Keys(ctx context.Context, namespace, name string) ([]string, error)
}

// The fixed keys of a ChatConnection's credentials Secret (spec section 7).
const (
	keyBotToken = "botToken"
	keyAppToken = "appToken"
)

// secretRef is one Secret that an object of a Set refers to, and what it must hold.
type secretRef struct {
	from identity // the object that names it; the Secret is in its namespace (D29)
	path string   // the field that names it
	name string
	keys []string // the keys the Secret must have
}

// secretRefs lists every Secret that set refers to: each ChatConnection's credentials, an
// AgentBackend's header Secrets, and a Binding's token. Resolve checks them, and
// SecretNames lists the Secrets off them, so the two cannot drift apart. An empty key
// means the default, for a Set that has not been through Default.
func (s *Set) secretRefs() []secretRef {
	var refs []secretRef
	keyRef := func(from identity, path string, ref *v1alpha1.SecretKeyRef) {
		if ref != nil {
			refs = append(refs, secretRef{from, path, ref.Name, []string{cmp.Or(ref.Key, v1alpha1.DefaultSecretKey)}})
		}
	}
	for i := range s.AgentBackends {
		be := &s.AgentBackends[i]
		for j, h := range be.Spec.Headers {
			keyRef(identity{"AgentBackend", be.Namespace, be.Name}, fmt.Sprintf("spec.headers[%d].valueSecretRef", j), h.ValueSecretRef)
		}
	}
	for i := range s.ChatConnections {
		c := &s.ChatConnections[i]
		refs = append(refs, secretRef{identity{"ChatConnection", c.Namespace, c.Name}, "spec.credentialsSecretRef", c.Spec.CredentialsSecretRef.Name, []string{keyBotToken, keyAppToken}})
	}
	for i := range s.Bindings {
		b := &s.Bindings[i]
		from := identity{"Binding", b.Namespace, b.Name}
		keyRef(from, "spec.identity.tokenSecretRef", b.Spec.Identity.TokenSecretRef)
	}
	return refs
}

// SecretName names one Secret.
type SecretName struct{ Namespace, Name string }

// String is namespace/name.
func (n SecretName) String() string { return n.Namespace + "/" + n.Name }

// secretNames returns the sorted, unique Secrets that refs name, each in the namespace of the object that names it.
func secretNames(refs []secretRef) []SecretName {
	names := make([]SecretName, 0, len(refs))
	for _, ref := range refs {
		names = append(names, SecretName{ref.from.namespace, ref.name})
	}
	slices.SortFunc(names, func(a, b SecretName) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return slices.Compact(names)
}

// SecretNames returns the sorted Secrets that set refers to, each once, which are the ones a SecretWatcher must follow.
func (s *Set) SecretNames() []SecretName { return secretNames(s.secretRefs()) }

// SecretWatcher is an optional interface of a SecretResolver that follows Secrets one by one, as the cluster-backed one
// does. The Reloader calls it around each reload, so that a resolver reads exactly the Secrets the config refers to: it
// holds nothing else, and a Secret that a reload adds is readable without a restart.
type SecretWatcher interface {
	// Watch starts following each of names that is not followed yet, and waits until each is readable or ctx ends. The
	// error says which is not and why. Resolve reports the same Secrets through Keys, so a caller that is about to
	// resolve may ignore it. The Secrets stay followed after ctx ends.
	Watch(ctx context.Context, names []SecretName) error
	// Retain keeps following exactly names (and whatever the resolver was told to keep itself) and stops following the
	// rest. The Reloader calls it with the Secrets of each snapshot it publishes, and never after a failed reload, so
	// the Secrets of the last good snapshot stay readable.
	Retain(names []SecretName)
}

// presentSecrets is a SecretResolver for which every Secret exists and has the keys given.
type presentSecrets []string

// Keys returns the keys every Secret has.
func (p presentSecrets) Keys(context.Context, string, string) ([]string, error) { return p, nil }

// PresentSecrets returns a SecretResolver for which every Secret that set refers to exists and has every key that set
// needs of any Secret: the two tokens of a ChatConnection's credentials, and the key of each Binding token and header
// Secret. It is for checking the references between a set's objects alone, as Resolve does, where the Secrets are
// created by whoever applies the set and no cluster is at hand. Never give it to Resolve for a config the broker
// runs: it hides a missing Secret or key that the broker must report.
func PresentSecrets(set *Set) SecretResolver {
	var keys presentSecrets
	for _, ref := range set.secretRefs() {
		for _, k := range ref.keys {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// checkSecrets checks every Secret that an object of set refers to.
func (r *resolver) checkSecrets(ctx context.Context, set *Set) {
	for _, ref := range set.secretRefs() {
		r.checkSecret(ctx, ref.from, ref.path, ref.name, ref.keys...)
	}
}

// checkSecret requires the Secret name, in the namespace of from, to have every one of keys.
// Once ctx ends it does nothing and records nothing, so a cancelled read is not a problem
// with the config; Resolve reports the cancellation once.
func (r *resolver) checkSecret(ctx context.Context, from identity, path, name string, keys ...string) {
	if ctx.Err() != nil {
		return
	}
	secret := identity{"Secret", from.namespace, name}
	have, err := r.secrets.Keys(ctx, secret.namespace, name)
	switch {
	case ctx.Err() != nil: // cancelled during the read; err may be that, not a real problem
	case errors.Is(err, ErrSecretNotFound):
		r.fail(from, path, fmt.Errorf("%s not found", secret))
	case err != nil:
		r.record(from, path, fmt.Errorf("read %s: %w", secret, err)) // the cache, not the config, failed
	default:
		for _, k := range keys {
			if !slices.Contains(have, k) {
				r.fail(from, path, fmt.Errorf("%s has no key %q", secret, k))
			}
		}
	}
}
