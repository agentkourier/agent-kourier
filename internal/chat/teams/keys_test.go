package teams

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stand-ins, of the shape and length of real ones, for the IDs of a tenant and its bot, and the tenant's service URL.
const (
	testTenant  = "11111111-2222-4333-8444-555555555555"
	testAppID   = "22222222-3333-4444-8555-666666666666"
	testService = "https://smba.trafficmanager.net/amer/11111111-2222-4333-8444-555555555555/"
)

// signingKey is a key the fake Bot Framework publishes, with the private half a test signs with.
type signingKey struct {
	kid          string
	priv         *rsa.PrivateKey
	endorsements []string
}

// newSigningKey makes a 2048-bit key: the verifier refuses smaller ones.
func newSigningKey(t testing.TB, kid string, endorsements ...string) signingKey {
	t.Helper()
	return newSigningKeyOf(t, 2048, kid, endorsements...)
}

// newSigningKeyOf makes a key of bits bits.
func newSigningKeyOf(t testing.TB, bits int, kid string, endorsements ...string) signingKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return signingKey{kid: kid, priv: priv, endorsements: endorsements}
}

// fakeBotFramework serves the OpenID metadata and the JWKS that the verifier reads, as login.botframework.com does, with keys a test
// can change, and counts the fetches of the JWKS.
type fakeBotFramework struct {
	srv *httptest.Server
	mux *http.ServeMux

	mu       sync.Mutex
	keys     []signingKey
	status   int // the status of the JWKS, 200 when 0
	padding  int // the length of a string field added to the JWKS, to make it large and still valid JSON
	jwksGets int
	jwksPath string // the path of the metadata's jwks_uri, /keys when ""
}

func newFakeBotFramework(t testing.TB, keys ...signingKey) *fakeBotFramework {
	t.Helper()
	f := &fakeBotFramework{keys: keys, mux: http.NewServeMux()}
	f.mux.HandleFunc("GET /openid", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		path := f.jwksPath
		f.mu.Unlock()
		if path == "" {
			path = "/keys"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": "https://api.botframework.com", "jwks_uri": f.srv.URL + path, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	f.mux.HandleFunc("GET /keys", f.serveKeys)
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotFramework) serveKeys(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jwksGets++
	if f.status != 0 && f.status != http.StatusOK {
		http.Error(w, "unavailable", f.status)
		return
	}
	set := make([]map[string]any, 0, len(f.keys))
	for _, k := range f.keys {
		set = append(set, map[string]any{
			"kty": "RSA", "use": "sig", "kid": k.kid, "endorsements": k.endorsements,
			"n": base64.RawURLEncoding.EncodeToString(k.priv.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.priv.E)).Bytes()),
		})
	}
	doc := map[string]any{"keys": set}
	if f.padding > 0 {
		doc["padding"] = strings.Repeat("x", f.padding)
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeBotFramework) metadataURL() string { return f.srv.URL + "/openid" }

func (f *fakeBotFramework) publish(keys ...signingKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeBotFramework) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jwksGets
}

func (f *fakeBotFramework) setStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 18, 1, 20, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// claimsAt are the claims of a token that Teams sends the test bot, valid at now. Real tokens spell the service URL's claim
// serviceurl.
func claimsAt(now time.Time) map[string]any {
	return map[string]any{
		"iss":        "https://api.botframework.com",
		"aud":        testAppID,
		"serviceurl": testService,
		"nbf":        now.Add(-time.Minute).Unix(),
		"exp":        now.Add(time.Hour).Unix(),
	}
}

// signToken makes a compact JWS of claims under header, signed RS256 with priv, or with an empty signature when priv is nil.
func signToken(t testing.TB, header, claims map[string]any, priv *rsa.PrivateKey) string {
	t.Helper()
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := seg(header) + "." + seg(claims)
	if priv == nil {
		return signed + "."
	}
	h := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// bearer is the Authorization header of a token signed with k.
func bearer(t testing.TB, k signingKey, claims map[string]any) string {
	t.Helper()
	return "Bearer " + signToken(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": k.kid}, claims, k.priv)
}

// newTestVerifier is a verifier of the test bot's tokens against f, on clock.
func newTestVerifier(f *fakeBotFramework, clock *fakeClock) *verifier {
	return newVerifier(testAppID, f.metadataURL(), f.srv.Client(), clock.now)
}

// verifyActivity checks authz as the route does, for an activity whose serviceUrl is serviceURL.
func verifyActivity(ctx context.Context, v *verifier, authz, serviceURL string) error {
	tok, err := v.verify(ctx, authz)
	if err != nil {
		return err
	}
	return tok.checkServiceURL(serviceURL)
}

// reasonOf is the Reason of err, a *TokenError, or "" for nil, and fails the test for any other error or one that quotes a token.
func reasonOf(t testing.TB, err error) Reason {
	t.Helper()
	if err == nil {
		return ""
	}
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is no *TokenError", err)
	}
	if strings.Contains(err.Error(), "eyJ") {
		t.Errorf("error %q quotes a token", err)
	}
	return te.Reason
}
