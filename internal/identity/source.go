package identity

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// Auditor records an agent call. *audit.Writer implements it.
type Auditor interface {
	AgentCall(ctx context.Context, a audit.Actor, c audit.Call) error
}

// Options configures a Source.
type Options struct {
	// Token replaces the static source that identity.tokenSecretRef would give. A Binding
	// with a Token always requires one.
	Token TokenSource
	// OnRejected is called once for each rejected credential, for a metric (spec D34,
	// section 11), with the Binding's namespace/name.
	OnRejected func(binding string)
}

// Source builds the credentials of one Binding: the bearer token, the userId, and the
// backend's static headers with their Secret values read. It is safe for concurrent use.
type Source struct {
	binding    string // namespace/name
	spec       *v1alpha1.Binding
	backend    *v1alpha1.AgentBackend
	reader     SecretReader
	token      TokenSource // nil when the Binding has no token
	audit      Auditor
	onRejected func(string)
}

// New returns the Source for b, which reaches its agent on backend. Secrets are read
// through r: the token from b's namespace, and each header's from backend's. aud records the
// rejections and must not be nil. b and backend are read on every call, so they must not
// change; pass the objects of a config snapshot.
func New(b *v1alpha1.Binding, backend *v1alpha1.AgentBackend, r SecretReader, aud Auditor, opts Options) *Source {
	s := &Source{
		binding: b.Namespace + "/" + b.Name, spec: b, backend: backend, reader: r,
		token: opts.Token, audit: aud, onRejected: opts.OnRejected,
	}
	if s.token == nil && b.Spec.Identity.TokenSecretRef != nil {
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

// Rejected is for the session manager to call with every failed agent call. When call.Err
// carries agent.ErrCredentialsRejected (the front door answered 401 or 403) it re-reads the
// token, writes an audit record of the call, and calls OnRejected; for any other error it
// does nothing. It never retries: the turn has failed, and the next one reads the Secret
// again.
//
// The returned error reports only this bookkeeping: the token that cannot be read now, and
// an audit write that failed (spec 8.6 step 7: the record is of something already done, so
// the caller logs the error and carries on). The re-read and the metric happen either way.
// actor.Binding defaults to the Binding's namespace/name.
func (s *Source) Rejected(ctx context.Context, actor audit.Actor, call audit.Call) error {
	if !errors.Is(call.Err, agent.ErrCredentialsRejected) {
		return nil
	}
	var errs []error
	if s.token != nil {
		errs = append(errs, s.token.Refresh(ctx))
	}
	actor.Binding = cmp.Or(actor.Binding, s.binding)
	errs = append(errs, s.audit.AgentCall(ctx, actor, call))
	if s.onRejected != nil {
		s.onRejected(s.binding)
	}
	return errors.Join(errs...)
}
