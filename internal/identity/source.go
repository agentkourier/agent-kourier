package identity

import (
	"cmp"
	"context"
	"fmt"
	"net/http"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// Source builds the credentials of one Binding: the bearer token, the userId, and the
// backend's static headers with their Secret values read. It is safe for concurrent use.
type Source struct {
	binding string // namespace/name
	spec    *v1alpha1.Binding
	backend *v1alpha1.AgentBackend
	reader  SecretReader
	token   TokenSource // nil when the Binding has no token
}

// New returns the Source for b, which reaches its agent on backend. Secrets are read
// through r: the token from b's namespace, and each header's from backend's. b and backend
// are read on every call, so they must not change; pass the objects of a config snapshot.
func New(b *v1alpha1.Binding, backend *v1alpha1.AgentBackend, r SecretReader) *Source {
	s := &Source{
		binding: b.Namespace + "/" + b.Name, spec: b, backend: backend, reader: r,
	}
	if b.Spec.Identity.TokenSecretRef != nil {
		s.token = NewStaticToken(r, b.Namespace, *b.Spec.Identity.TokenSecretRef)
	}
	return s
}

// String names the Binding and nothing else. A Source holds the Secret reader, and fmt would
// otherwise print what the reader holds: %s and %q of a pointer show the fields it points to.
func (s *Source) String() string { return "identity.Source{" + s.binding + "}" }

// GoString is String, so %#v is safe.
func (s *Source) GoString() string { return s.String() }

// TokenRequired reports whether the Binding has a token, so that credentials without one
// must fail rather than fall back to the userId.
func (s *Source) TokenRequired() bool { return s.token != nil }

// Target returns the agent.Target for the Binding's agent, with the credentials func and
// TokenRequired set.
func (s *Source) Target() agent.Target {
	return agent.Target{
		Binding:       s.binding,
		Backend:       s.backend,
		Agent:         s.spec.Spec.Agent,
		Credentials:   s.Credentials,
		TokenRequired: s.TokenRequired(),
	}
}

// Credentials reads the Binding's current credentials. It is an agent.CredentialsFunc.
//
// If the token or a header's Secret cannot be read it returns empty credentials and an error
// wrapping ErrTokenUnreadable or ErrHeaderUnreadable (and the cause), and the call must fail:
// the userId is never sent in place of a token.
func (s *Source) Credentials(ctx context.Context) (dialect.Credentials, error) {
	c := dialect.Credentials{UserID: s.spec.Spec.Identity.UserID}
	if s.token != nil {
		tok, err := s.token.Token(ctx)
		if err != nil {
			return dialect.Credentials{}, err
		}
		c.Token = tok.Reveal()
	}
	for _, h := range s.backend.Spec.Headers {
		val := h.Value
		if ref := h.ValueSecretRef; ref != nil {
			key := cmp.Or(ref.Key, v1alpha1.DefaultSecretKey)
			v, err := s.reader.Value(ctx, s.backend.Namespace, ref.Name, key)
			if err != nil {
				return dialect.Credentials{}, fmt.Errorf("%w: header %q from secret %s/%s key %q: %w",
					ErrHeaderUnreadable, h.Name, s.backend.Namespace, ref.Name, key, err)
			}
			val = v.Reveal()
		}
		if c.Headers == nil {
			c.Headers = http.Header{}
		}
		c.Headers.Add(h.Name, val)
	}
	return c, nil
}
