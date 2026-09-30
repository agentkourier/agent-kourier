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
	"github.com/agentkourier/agent-kourier/internal/secret"
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
	identity   string      // what an audit row names as the identity, never the token itself
	audit      Auditor
	onRejected func(string)
}

// New returns the Source for b, which reaches its agent on backend. Secrets are read
// through r: the token from b's namespace, and each header's from backend's. aud records the
// rejections; New fails when it is nil, rather than at the first rejection. b and backend are
// read on every call, so they must not change; pass the objects of a config snapshot.
func New(b *v1alpha1.Binding, backend *v1alpha1.AgentBackend, r SecretReader, aud Auditor, opts Options) (*Source, error) {
	if aud == nil {
		return nil, errors.New("identity: New needs an Auditor")
	}
	s := &Source{
		binding: b.Namespace + "/" + b.Name, spec: b, backend: backend, reader: r,
		token: opts.Token, audit: aud, onRejected: opts.OnRejected,
	}
	// The identity is the token's owner, which only the token knows. Name the Secret that
	// holds it, or, when no token is sent, the userId that is.
	s.identity = b.Spec.Identity.UserID
	if ref := b.Spec.Identity.TokenSecretRef; ref != nil {
		s.identity = "secret:" + b.Namespace + "/" + ref.Name
		if s.token == nil {
			s.token = NewStaticToken(r, b.Namespace, *ref)
		}
	}
	return s, nil
}

// String names the Binding and nothing else. A Source holds the Secret reader, and fmt would
// otherwise print what the reader holds: %s and %q of a pointer show the fields it points to.
// It has a value receiver so that a copy is redacted too.
func (s Source) String() string { return "identity.Source{" + s.binding + "}" }

// GoString is String, so %#v is safe.
func (s Source) GoString() string { return s.String() }

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
// wrapping agent.ErrTokenUnreadable or agent.ErrHeaderUnreadable (and the cause), and the call must fail:
// the userId is never sent in place of a token.
func (s *Source) Credentials(ctx context.Context) (dialect.Credentials, error) {
	c := dialect.Credentials{UserID: s.spec.Spec.Identity.UserID}
	if s.token != nil {
		tok, err := s.token.Token(ctx)
		if err != nil {
			return dialect.Credentials{}, err
		}
		if tok.Reveal() == "" { // whatever the source, an empty token must not leave the userId
			return dialect.Credentials{}, fmt.Errorf("%w: %w", agent.ErrTokenUnreadable, secret.ErrEmptyValue)
		}
		c.Token = tok.Reveal()
	}
	for _, h := range s.backend.Spec.Headers {
		val := h.Value
		if ref := h.ValueSecretRef; ref != nil {
			key := cmp.Or(ref.Key, v1alpha1.DefaultSecretKey)
			v, err := s.reader.Value(ctx, s.backend.Namespace, ref.Name, key)
			if err == nil {
				err = checkValue(v, s.backend.Namespace, ref.Name, key)
			}
			if err != nil {
				return dialect.Credentials{}, fmt.Errorf("%w: header %q: %w", agent.ErrHeaderUnreadable, h.Name, err)
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
// This is the only audit write for a rejected call: the caller must not also audit it.
// call names the operation and its message, context or task ID, as audit.Writer.AgentCall
// needs. actor.Binding defaults to the Binding's namespace/name, and actor.Identity to
// "secret:namespace/name" of the token Secret, or to the userId of a Binding without one.
//
// The returned error reports only this bookkeeping: the token that cannot be read now, and
// an audit write that failed (spec 8.6 step 7: the record is of something already done, so
// it never blocks the follow-up). The caller must log it: it is the only place an operator
// sees that the token is now unreadable. The re-read and the metric happen either way.
func (s *Source) Rejected(ctx context.Context, actor audit.Actor, call audit.Call) error {
	if !errors.Is(call.Err, agent.ErrCredentialsRejected) {
		return nil
	}
	var errs []error
	if s.token != nil {
		errs = append(errs, s.token.Refresh(ctx))
	}
	actor.Binding = cmp.Or(actor.Binding, s.binding)
	actor.Identity = cmp.Or(actor.Identity, s.identity)
	errs = append(errs, s.audit.AgentCall(ctx, actor, call))
	if s.onRejected != nil {
		s.onRejected(s.binding)
	}
	return errors.Join(errs...)
}
