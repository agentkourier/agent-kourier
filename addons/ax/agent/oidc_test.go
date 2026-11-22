package agent

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestOIDCDiscoveryIsLazyAndIdentityIsVerified(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var available atomic.Bool
	var keysAvailable atomic.Bool
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !available.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		if !keysAvailable.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	issuer = server.URL
	verify, err := OIDCAuthenticate(context.Background(), issuer, "agent-kourier", "service")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("constructor contacted the issuer; golden snapshot startup requires no egress")
	}
	if _, err := verify(t.Context(), ""); err == nil {
		t.Fatal("missing bearer accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("unauthenticated request triggered discovery")
	}
	token := func(iss, aud, sub string, expiry time.Time) string {
		t.Helper()
		claims, _ := json.Marshal(map[string]any{"iss": iss, "aud": aud, "sub": sub, "exp": expiry.Unix(), "iat": time.Now().Add(-time.Minute).Unix()})
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test","typ":"JWT"}`))
		payload := header + "." + base64.RawURLEncoding.EncodeToString(claims)
		digest := sha256.Sum256([]byte(payload))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	good := token(issuer, "agent-kourier", "service", time.Now().Add(time.Hour))
	if _, err := verify(t.Context(), good); !errors.Is(err, ErrAuthenticationUnavailable) {
		t.Fatalf("unavailable issuer error = %v, want retryable authentication outage", err)
	}
	m := &fakeModel{}
	agent, err := New(t.Context(), Config{StateDir: t.TempDir(), Model: m, Authenticate: verify})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	checkHTTP := func(path, header string, status int) {
		t.Helper()
		method := http.MethodPost
		if strings.Contains(path, "/.well-known/") {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{"jsonrpc":"2.0","id":"request","method":"message/send"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		agent.Handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s status = %d, want %d: %s", path, w.Code, status, w.Body.String())
		}
		if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") != "1" {
			t.Fatal("outage response omitted retry interval")
		}
	}
	for _, path := range []string{"/a2a/.well-known/agent-card.json", "/a2a"} {
		checkHTTP(path, good, http.StatusServiceUnavailable)
		checkHTTP(path, "", http.StatusUnauthorized)
	}
	if m.calls.Load() != 0 {
		t.Fatal("outage dispatched model work")
	}
	available.Store(true)
	// Discovery can recover while the signing-key endpoint is still unavailable.
	// This must also be retryable, on both the card and RPC paths.
	for _, path := range []string{"/a2a/.well-known/agent-card.json", "/a2a"} {
		checkHTTP(path, good, http.StatusServiceUnavailable)
	}
	keysAvailable.Store(true)
	if sub, err := verify(t.Context(), good); err != nil || sub != "service" {
		t.Fatalf("discovery did not recover: sub=%q error=%v", sub, err)
	}
	checkHTTP("/a2a/.well-known/agent-card.json", good, http.StatusOK)
	agentHTTP := httptest.NewServer(agent.Handler)
	defer agentHTTP.Close()
	c := connect(t, agentHTTP, strings.TrimPrefix(good, "Bearer "))
	completed := send(t, c, message("after-outage", "hello"))
	if completed.Status.State != a2a.TaskStateCompleted || m.calls.Load() != 1 {
		t.Fatalf("request did not recover after outage: task=%+v calls=%d", completed, m.calls.Load())
	}
	for _, tc := range []struct {
		name, iss, aud, sub string
		expiry              time.Time
	}{
		{"issuer", issuer + "/other", "agent-kourier", "service", time.Now().Add(time.Hour)},
		{"audience", issuer, "other", "service", time.Now().Add(time.Hour)},
		{"subject", issuer, "agent-kourier", "other", time.Now().Add(time.Hour)},
		{"expiry", issuer, "agent-kourier", "service", time.Now().Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := token(tc.iss, tc.aud, tc.sub, tc.expiry)
			if _, err := verify(t.Context(), bad); err == nil || errors.Is(err, ErrAuthenticationUnavailable) {
				t.Fatalf("invalid identity error = %v", err)
			}
			for _, path := range []string{"/a2a/.well-known/agent-card.json", "/a2a"} {
				checkHTTP(path, bad, http.StatusUnauthorized)
			}
		})
	}
	forged := good[:len(good)-12] + "AAAAAAAAAAAA"
	if _, err := verify(t.Context(), forged); err == nil {
		t.Fatal("forged signature accepted")
	}
	for _, path := range []string{"/a2a/.well-known/agent-card.json", "/a2a"} {
		checkHTTP(path, forged, http.StatusUnauthorized)
	}
}
