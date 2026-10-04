package agent

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrAuthenticationUnavailable means the identity provider could not be reached.
// Requests must fail closed with a retryable response, not a credential rejection.
var ErrAuthenticationUnavailable = errors.New("authentication unavailable")

// OIDCAuthenticate verifies issuer, audience and the configured service subject for every request.
// Discovery is lazy: building an AX golden snapshot must not require actor-specific egress.
func OIDCAuthenticate(_ context.Context, issuer, audience, subject string) (func(context.Context, string) (string, error), error) {
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || audience == "" || subject == "" {
		return nil, errors.New("valid OIDC issuer, audience and subject are required")
	}
	var mu sync.Mutex
	var verifier *oidc.IDTokenVerifier
	client := &http.Client{Timeout: 10 * time.Second, Transport: oidcTransport{http.DefaultTransport}}
	return func(ctx context.Context, header string) (string, error) {
		token, err := VerifyBearer(header)
		if err != nil {
			return "", err
		}
		ctx = oidc.ClientContext(ctx, client)
		mu.Lock()
		if verifier == nil {
			provider, err := oidc.NewProvider(ctx, issuer)
			if err != nil {
				mu.Unlock()
				return "", ErrAuthenticationUnavailable
			}
			var discovery struct {
				KeysURL    string   `json:"jwks_uri"`
				Algorithms []string `json:"id_token_signing_alg_values_supported"`
			}
			if err := provider.Claims(&discovery); err != nil || discovery.KeysURL == "" {
				mu.Unlock()
				return "", ErrAuthenticationUnavailable
			}
			keys := &oidcKeys{oidc.NewRemoteKeySet(ctx, discovery.KeysURL)}
			verifier = oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience, SupportedSigningAlgs: discovery.Algorithms})
		}
		v := verifier
		mu.Unlock()
		// go-oidc formats signature errors without wrapping them. Capture a
		// key-fetch outage separately so invalid signatures remain 401 responses.
		var unavailable bool
		ctx = context.WithValue(ctx, oidcFailureKey{}, &unavailable)
		id, err := v.Verify(ctx, token)
		if err != nil && unavailable {
			return "", ErrAuthenticationUnavailable
		}
		if err != nil || id.Subject != subject {
			return "", errors.New("invalid service identity")
		}
		return id.Subject, nil
	}, nil
}

// oidcTransport marks failed issuer requests without exposing upstream response bodies.
type oidcTransport struct{ base http.RoundTripper }

func (t oidcTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, ErrAuthenticationUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, ErrAuthenticationUnavailable
	}
	return response, nil
}

type oidcFailureKey struct{}
type oidcKeys struct{ inner oidc.KeySet }

func (k *oidcKeys) VerifySignature(ctx context.Context, token string) ([]byte, error) {
	payload, err := k.inner.VerifySignature(ctx, token)
	if errors.Is(err, ErrAuthenticationUnavailable) {
		if unavailable, ok := ctx.Value(oidcFailureKey{}).(*bool); ok {
			*unavailable = true
		}
	}
	return payload, err
}
