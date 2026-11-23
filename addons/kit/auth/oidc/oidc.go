// Package oidc is the caller check's OIDC verifier, apart from package auth so that only an add-on that offers OIDC
// links go-oidc and its dependencies.
package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

// Timeout bounds each request to the issuer: discovery and the signing keys.
const Timeout = 10 * time.Second

// Verifier accepts a request whose Authorization carries an ID token from the issuer, for the audience, whose subject is
// the one configured, as that subject. Discovery is lazy, at the first request with a credential, so that the add-on
// starts without egress to the issuer, which building an AX golden snapshot needs. An issuer or key set that cannot be
// reached is auth.ErrUnavailable.
type Verifier struct {
	issuer, audience, subject string
	client                    *http.Client

	mu       sync.Mutex
	verifier *gooidc.IDTokenVerifier
}

// New returns the OIDC verifier. It contacts no one.
func New(issuer, audience, subject string) (*Verifier, error) {
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || audience == "" || subject == "" {
		return nil, errors.New("valid OIDC issuer, audience and subject are required")
	}
	return &Verifier{issuer: issuer, audience: audience, subject: subject,
		client: &http.Client{Timeout: Timeout, Transport: oidcTransport{http.DefaultTransport}}}, nil
}

// Verify implements auth.Verifier.
func (o *Verifier) Verify(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	switch len(values) {
	case 0:
		return "", auth.ErrMissing
	case 1:
	default:
		return "", auth.ErrInvalid
	}
	token, err := auth.Bearer(values[0])
	if err != nil {
		return "", err
	}
	v, err := o.discover(r.Context())
	if err != nil {
		return "", err
	}
	// go-oidc formats signature errors without wrapping them. Capture a key-fetch outage separately so invalid
	// signatures remain 401 responses.
	var unavailable bool
	ctx := context.WithValue(gooidc.ClientContext(r.Context(), o.client), oidcFailureKey{}, &unavailable)
	id, err := v.Verify(ctx, token)
	if err != nil && unavailable {
		return "", auth.ErrUnavailable
	}
	if err != nil || id.Subject != o.subject {
		return "", errors.New("invalid service identity")
	}
	return id.Subject, nil
}

// discover returns the verifier, reading the issuer's discovery document the first time it succeeds.
func (o *Verifier) discover(ctx context.Context) (*gooidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier != nil {
		return o.verifier, nil
	}
	ctx = gooidc.ClientContext(ctx, o.client)
	provider, err := gooidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return nil, auth.ErrUnavailable
	}
	var discovery struct {
		KeysURL    string   `json:"jwks_uri"`
		Algorithms []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&discovery); err != nil || discovery.KeysURL == "" {
		return nil, auth.ErrUnavailable
	}
	// The key set keeps ctx for its client, and drops its cancel: a key fetched after this request has ended still runs.
	keys := &oidcKeys{gooidc.NewRemoteKeySet(ctx, discovery.KeysURL)}
	o.verifier = gooidc.NewVerifier(o.issuer, keys, &gooidc.Config{ClientID: o.audience, SupportedSigningAlgs: discovery.Algorithms})
	return o.verifier, nil
}

// oidcTransport marks failed issuer requests without exposing upstream response bodies.
type oidcTransport struct{ base http.RoundTripper }

func (t oidcTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, auth.ErrUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, auth.ErrUnavailable
	}
	return response, nil
}

type oidcFailureKey struct{}

// oidcKeys records, in the verification's context, that the key set could not be fetched.
type oidcKeys struct{ inner gooidc.KeySet }

func (k *oidcKeys) VerifySignature(ctx context.Context, token string) ([]byte, error) {
	payload, err := k.inner.VerifySignature(ctx, token)
	if errors.Is(err, auth.ErrUnavailable) {
		if unavailable, ok := ctx.Value(oidcFailureKey{}).(*bool); ok {
			*unavailable = true
		}
	}
	return payload, err
}
