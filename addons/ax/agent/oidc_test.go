package agent

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/ax/auth"
)

// The AX route's caller check, end to end: while the issuer is down, the card and the endpoint answer a retryable 503
// and no turn runs; once it is back, the same token runs a turn.
func TestOIDCOutageIsRetryableAndRecovers(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	issuer = server.URL
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "agent-kourier", "sub": "service", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix()})
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := payload + "." + base64.RawURLEncoding.EncodeToString(signature)

	verifier, err := auth.NewOIDC(issuer, "agent-kourier", "service")
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{}
	a := startWith(t, t.TempDir(), m, &auth.Check{Verifier: verifier, Bearer: true, Log: discard()})
	check := func(path string, status int) {
		t.Helper()
		method := http.MethodPost
		if strings.HasSuffix(path, CardPath) {
			method = http.MethodGet
		}
		req, err := http.NewRequestWithContext(t.Context(), method, a.http.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":"request","method":"message/send"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := a.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("%s status = %d, want %d", path, resp.StatusCode, status)
		}
		if status == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != "1" {
			t.Fatal("outage response omitted retry interval")
		}
	}
	for _, path := range []string{A2APath + CardPath, A2APath} {
		check(path, http.StatusServiceUnavailable)
	}
	if m.calls.Load() != 0 {
		t.Fatal("outage dispatched model work")
	}
	available.Store(true)
	check(A2APath+CardPath, http.StatusOK)
	completed := send(t, connect(t, a.http, token), message("after-outage", "hello"))
	if completed.Status.State != a2a.TaskStateCompleted || m.calls.Load() != 1 {
		t.Fatalf("request did not recover after outage: task=%+v calls=%d", completed, m.calls.Load())
	}
}
