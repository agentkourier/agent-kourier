package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
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
	keyBotToken      = "botToken"
	keyAppToken      = "appToken"
	keySigningSecret = "signingSecret"
)

// SecretNamespaces returns the sorted namespaces whose Secrets set refers to, which are the ones
// a SecretResolver must be given (D29: a Secret lives in the namespace of the object that names
// it). It follows the references checkSecrets reads: every ChatConnection's credentials, an
// AgentBackend's header Secrets, and a Binding's token and webhook token.
func (s *Set) SecretNamespaces() []string {
	seen := map[string]bool{}
	for i := range s.AgentBackends {
		for _, h := range s.AgentBackends[i].Spec.Headers {
			if h.ValueSecretRef != nil {
				seen[s.AgentBackends[i].Namespace] = true
			}
		}
	}
	for i := range s.ChatConnections {
		seen[s.ChatConnections[i].Namespace] = true
	}
	for i := range s.Bindings {
		if b := &s.Bindings[i]; b.Spec.Identity.TokenSecretRef != nil || b.Spec.Alerts != nil {
			seen[b.Namespace] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// checkSecrets checks every Secret that an object of set refers to. Each lives in the
// referring object's namespace (D29).
func (r *resolver) checkSecrets(ctx context.Context, set *Set) {
	for i := range set.AgentBackends {
		be := &set.AgentBackends[i]
		for j, h := range be.Spec.Headers {
			r.checkKeyRef(ctx, identity{"AgentBackend", be.Namespace, be.Name}, fmt.Sprintf("spec.headers[%d].valueSecretRef", j), h.ValueSecretRef)
		}
	}
	for i := range set.ChatConnections {
		c := &set.ChatConnections[i]
		keys := []string{keyBotToken, keyAppToken}
		if c.Spec.Mode == v1alpha1.ModeHTTP {
			keys[1] = keySigningSecret
		}
		r.checkSecret(ctx, identity{"ChatConnection", c.Namespace, c.Name}, "spec.credentialsSecretRef", c.Spec.CredentialsSecretRef.Name, keys...)
	}
	for i := range set.Bindings {
		b := &set.Bindings[i]
		from := identity{"Binding", b.Namespace, b.Name}
		r.checkKeyRef(ctx, from, "spec.identity.tokenSecretRef", b.Spec.Identity.TokenSecretRef)
		if a := b.Spec.Alerts; a != nil {
			r.checkKeyRef(ctx, from, "spec.alerts.tokenSecretRef", &a.TokenSecretRef)
		}
	}
}

// checkKeyRef checks a single-value reference, which may be unset. An empty key means the
// default, for a Set that has not been through Default.
func (r *resolver) checkKeyRef(ctx context.Context, from identity, path string, ref *v1alpha1.SecretKeyRef) {
	if ref != nil {
		r.checkSecret(ctx, from, path, ref.Name, cmp.Or(ref.Key, v1alpha1.DefaultSecretKey))
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
		r.fail(from, path, fmt.Errorf("read %s: %w", secret, err))
	default:
		for _, k := range keys {
			if !slices.Contains(have, k) {
				r.fail(from, path, fmt.Errorf("%s has no key %q", secret, k))
			}
		}
	}
}
