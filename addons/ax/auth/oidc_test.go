package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// issuer is a fake OIDC issuer whose discovery document and key set can be taken down, and whose keys can rotate.
type issuer struct {
	t         *testing.T
	srv       *httptest.Server
	requests  atomic.Int32
	available atomic.Bool
	keysUp    atomic.Bool

	mu   sync.Mutex
	keys map[string]*rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{t: t, keys: map[string]*rsa.PrivateKey{}}
	is.addKey("k1")
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		is.requests.Add(1)
		if !is.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": is.srv.URL, "jwks_uri": is.srv.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		if !is.keysUp.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		is.mu.Lock()
		var keys []any
		for kid, key := range is.keys {
			keys = append(keys, map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"})
		}
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) addKey(kid string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		is.t.Fatal(err)
	}
	is.mu.Lock()
	is.keys[kid] = key
	is.mu.Unlock()
}

// token is "Bearer <ID token>" signed with the key kid.
func (is *issuer) token(kid, iss, aud, sub string, expiry time.Time) string {
	is.t.Helper()
	is.mu.Lock()
	key := is.keys[kid]
	is.mu.Unlock()
	claims, _ := json.Marshal(map[string]any{"iss": iss, "aud": aud, "sub": sub, "exp": expiry.Unix(), "iat": time.Now().Add(-time.Minute).Unix()})
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"` + kid + `","typ":"JWT"}`))
	payload := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		is.t.Fatal(err)
	}
	return "Bearer " + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func request(ctx context.Context, authorization ...string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/a2a", nil)
	for _, v := range authorization {
		r.Header.Add("Authorization", v)
	}
	return r
}

func TestOIDCDiscoveryIsLazyAndIdentityIsVerified(t *testing.T) {
	is := newIssuer(t)
	v, err := NewOIDC(is.srv.URL, "agent-kourier", "service")
	if err != nil {
		t.Fatal(err)
	}
	if is.requests.Load() != 0 {
		t.Fatal("NewOIDC contacted the issuer; golden snapshot startup requires no egress")
	}
	if _, err := v.Verify(request(t.Context())); !errors.Is(err, ErrMissing) {
		t.Fatalf("no credential: %v, want ErrMissing", err)
	}
	if is.requests.Load() != 0 {
		t.Fatal("a request without a credential triggered discovery")
	}
	good := is.token("k1", is.srv.URL, "agent-kourier", "service", time.Now().Add(time.Hour))
	if _, err := v.Verify(request(t.Context(), good, good)); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("two Authorization headers: %v, want invalid", err)
	}
	if _, err := v.Verify(request(t.Context(), good)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("issuer down: %v, want ErrUnavailable", err)
	}
	is.available.Store(true)
	// Discovery can recover while the signing-key endpoint is still unavailable: still retryable.
	if _, err := v.Verify(request(t.Context(), good)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("keys down: %v, want ErrUnavailable", err)
	}
	is.keysUp.Store(true)
	if sub, err := v.Verify(request(t.Context(), good)); err != nil || sub != "service" {
		t.Fatalf("after recovery: %q, %v; want service", sub, err)
	}
	for _, tc := range []struct {
		name, iss, aud, sub string
		expiry              time.Time
	}{
		{"issuer", is.srv.URL + "/other", "agent-kourier", "service", time.Now().Add(time.Hour)},
		{"audience", is.srv.URL, "other", "service", time.Now().Add(time.Hour)},
		{"subject", is.srv.URL, "agent-kourier", "other", time.Now().Add(time.Hour)},
		{"expiry", is.srv.URL, "agent-kourier", "service", time.Now().Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := is.token("k1", tc.iss, tc.aud, tc.sub, tc.expiry)
			if _, err := v.Verify(request(t.Context(), bad)); err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMissing) {
				t.Fatalf("invalid identity: %v, want invalid", err)
			}
		})
	}
	forged := good[:len(good)-12] + "AAAAAAAAAAAA"
	if _, err := v.Verify(request(t.Context(), forged)); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("forged signature: %v, want invalid", err)
	}
}

// The check answers an outage with a retryable 503, and a bad identity with 401 and the Bearer challenge.
func TestOIDCCheckResponses(t *testing.T) {
	is := newIssuer(t)
	v, err := NewOIDC(is.srv.URL, "agent-kourier", "service")
	if err != nil {
		t.Fatal(err)
	}
	check := func() *Check { return &Check{Verifier: v, Bearer: true} }
	good := is.token("k1", is.srv.URL, "agent-kourier", "service", time.Now().Add(time.Hour))
	bad := is.token("k1", is.srv.URL, "agent-kourier", "intruder", time.Now().Add(time.Hour))
	rec := httptest.NewRecorder()
	c := check()
	c.Log, _ = newLog()
	c.Wrap(http.NotFoundHandler()).ServeHTTP(rec, request(t.Context(), good))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("outage = %d Retry-After %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	status, _, _, reason := serve(t, check(), map[string]string{"Authorization": good})
	if status != http.StatusServiceUnavailable || reason != ReasonUnavailable {
		t.Fatalf("outage = %d %q, want 503 unavailable", status, reason)
	}
	is.available.Store(true)
	is.keysUp.Store(true)
	status, _, user, reason := serve(t, check(), map[string]string{"Authorization": good})
	if status != http.StatusOK || reason != "" || user == nil || user.Name != "service" {
		t.Fatalf("good = %d %q %+v, want 200 as service", status, reason, user)
	}
	status, challenge, _, reason := serve(t, check(), map[string]string{"Authorization": bad})
	if status != http.StatusUnauthorized || challenge != "Bearer" || reason != ReasonInvalid {
		t.Fatalf("bad subject = %d %q %q, want 401 Bearer invalid", status, challenge, reason)
	}
	status, _, _, reason = serve(t, check(), nil)
	if status != http.StatusUnauthorized || reason != ReasonMissing {
		t.Fatalf("missing = %d %q, want 401 missing", status, reason)
	}
}

// The key set outlives the request that discovered it: a key the issuer rotates in later is fetched, though the first
// request's context has long been cancelled.
func TestOIDCFetchesRotatedKeysAfterTheFirstRequestEnds(t *testing.T) {
	is := newIssuer(t)
	is.available.Store(true)
	is.keysUp.Store(true)
	v, err := NewOIDC(is.srv.URL, "agent-kourier", "service")
	if err != nil {
		t.Fatal(err)
	}
	first, cancel := context.WithCancel(context.Background())
	if _, err := v.Verify(request(first, is.token("k1", is.srv.URL, "agent-kourier", "service", time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	cancel()
	is.addKey("k2")
	rotated := is.token("k2", is.srv.URL, "agent-kourier", "service", time.Now().Add(time.Hour))
	if sub, err := v.Verify(request(t.Context(), rotated)); err != nil || sub != "service" {
		t.Fatalf("rotated key: %q, %v; want service", sub, err)
	}
}

func TestNewOIDCRefuses(t *testing.T) {
	for name, args := range map[string][3]string{
		"relative issuer": {"issuer.example.test", "aud", "sub"},
		"userinfo":        {"https://u:p@issuer.example.test", "aud", "sub"},
		"query":           {"https://issuer.example.test?x=1", "aud", "sub"},
		"no audience":     {"https://issuer.example.test", "", "sub"},
		"no subject":      {"https://issuer.example.test", "aud", ""},
	} {
		if _, err := NewOIDC(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: NewOIDC accepted it", name)
		}
	}
}
