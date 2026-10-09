// Package identity supplies the credentials a Binding's turns run with (spec 8.5, D34): the
// bearer token from identity.tokenSecretRef and the AgentBackend's static headers, whose
// values may come from Secrets.
//
// Nothing here caches a value. Every call reads the Secret again, through a reader that is
// itself served from a watch, so a token rotated outside Agent Kourier is used by the next call. A
// value stays a secret.Value until it is put in dialect.Credentials, and no error, log line or
// audit row carries one.
package identity

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// ErrControlCharacter is what a Secret value with a control character wraps, most often the
// trailing newline that kubectl create secret --from-file keeps. The value is never trimmed:
// a token that differs from what the operator stored is not the operator's to guess at.
// Neither the Authorization header nor any other can carry it.
var ErrControlCharacter = errors.New("identity: the value contains a control character")

// checkValue returns an error, naming the Secret and key and never the value, when v cannot
// be sent: it is empty, or holds a control character. The reader's own errors name them, so
// this is the only place that has to.
func checkValue(v secret.Value, namespace, name, key string) error {
	switch s := v.Reveal(); {
	case s == "":
		return fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secret.ErrEmptyValue)
	case strings.ContainsFunc(s, unicode.IsControl):
		return fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, ErrControlCharacter)
	}
	return nil
}

// SecretReader reads one value of a Secret. *k8ssecrets.Resolver implements it. An empty key
// means v1alpha1.DefaultSecretKey.
type SecretReader interface {
	Value(ctx context.Context, namespace, name, key string) (secret.Value, error)
}

// TokenSource supplies a Binding's bearer token. The static source reads a Secret; a source
// that mints tokens with the client-credentials grant plugs in here later (D34).
type TokenSource interface {
	// Token returns the current token. It is called for every operation, so an
	// implementation that caches must expire what it caches. On failure the error wraps
	// agent.ErrTokenUnreadable.
	Token(ctx context.Context) (secret.Value, error)
	// Refresh drops anything cached and reads the token again. It is called after the
	// backend rejected the token, and returns the error a fresh Token would.
	Refresh(ctx context.Context) error
}

// StaticToken is a TokenSource over one key of a Secret.
type StaticToken struct {
	reader    SecretReader
	namespace string
	ref       v1alpha1.SecretKeyRef
}

var _ TokenSource = (*StaticToken)(nil)

// NewStaticToken returns a StaticToken for ref, a Secret in namespace, read through r.
func NewStaticToken(r SecretReader, namespace string, ref v1alpha1.SecretKeyRef) *StaticToken {
	return &StaticToken{reader: r, namespace: namespace, ref: ref}
}

// String names the Secret, never the reader that holds its value, so that no fmt verb prints
// it. It has a value receiver so that a copy is redacted too.
func (s StaticToken) String() string {
	return fmt.Sprintf("identity.StaticToken{%s/%s}", s.namespace, s.ref.Name)
}

// GoString is String, so %#v is safe.
func (s StaticToken) GoString() string { return s.String() }

// Token reads the Secret. Every call reads it again, so a rotated Secret is used at once.
func (s *StaticToken) Token(ctx context.Context) (secret.Value, error) {
	key := cmp.Or(s.ref.Key, v1alpha1.DefaultSecretKey)
	v, err := s.reader.Value(ctx, s.namespace, s.ref.Name, key)
	if err == nil {
		err = checkValue(v, s.namespace, s.ref.Name, key)
	}
	if err != nil {
		return secret.Value{}, fmt.Errorf("%w: %w", agent.ErrTokenUnreadable, err)
	}
	return v, nil
}

// Refresh reads the Secret again. Nothing is cached, so this only reports whether the token
// can be read now.
func (s *StaticToken) Refresh(ctx context.Context) error {
	_, err := s.Token(ctx)
	return err
}
