package teams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// testClientSecret is the bot's fake client secret: obviously not one.
const testClientSecret = "fake-client-secret-not-real"

// fakeEntra is Microsoft Entra's token endpoint: it issues "fake-token-N" for the N-th grant it takes, or answers what fail says.
type fakeEntra struct {
	srv   *httptest.Server
	life  atomic.Int64 // the expires_in it gives, in seconds
	calls atomic.Int32
	// gate, if set, holds each answer until it is closed.
	gate chan struct{}

	mu   sync.Mutex
	fail func(w http.ResponseWriter) bool // guarded by mu: answers instead of issuing, when it returns true
	form []map[string]string              // guarded by mu: what each request asked
}

func newFakeEntra(t testing.TB) *fakeEntra {
	t.Helper()
	f := &fakeEntra{}
	f.life.Store(3599)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.gate != nil {
			<-f.gate
		}
		n := f.calls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		got := map[string]string{"path": r.URL.Path}
		for k := range r.PostForm {
			got[k] = r.PostForm.Get(k)
		}
		f.form = append(f.form, got)
		fail := f.fail
		f.mu.Unlock()
		if fail != nil && fail(w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":%d,"ext_expires_in":%d,"access_token":"fake-token-%d"}`, f.life.Load(), f.life.Load(), n)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEntra) failWith(fn func(w http.ResponseWriter) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}

func (f *fakeEntra) tokenURL() string { return f.srv.URL + "/" + testTenant + "/oauth2/v2.0/token" }

func (f *fakeEntra) source(clock *fakeClock) *tokenSource {
	return newTokenSource(f.tokenURL(), testAppID, botFrameworkScope, secret.New(testClientSecret), noRedirects(f.srv.Client()), clock.now)
}

func mustToken(t *testing.T, s *tokenSource) string {
	t.Helper()
	tok, err := s.token(t.Context())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return tok.Reveal()
}

// The first call fetches a token by the client credentials grant, for the Bot Framework's scope, and later calls are given it.
func TestTokenIsFetchedOnceAndKept(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	s := f.source(clock)
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token = %q", got)
	}
	clock.advance(50 * time.Minute)
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token after 50m = %q, want the kept one", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("Entra was asked %d times, want 1", n)
	}
	f.mu.Lock()
	form := f.form[0]
	f.mu.Unlock()
	want := map[string]string{"path": "/" + testTenant + "/oauth2/v2.0/token", "grant_type": "client_credentials",
		"client_id": testAppID, "client_secret": testClientSecret, "scope": "https://api.botframework.com/.default"}
	for k, v := range want {
		if form[k] != v {
			t.Errorf("request %s = %q, want %q", k, form[k], v)
		}
	}
}

// Within the refresh margin the kept token is still given at once, and a fetch begins in the background; once it lands, its token is
// given. Past the stale margin a caller waits for a new one.
func TestTokenIsRefreshedEarly(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	s := f.source(clock)
	mustToken(t, s)
	clock.advance(3599*time.Second - tokenRefreshMargin + time.Second)
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token in the refresh margin = %q, want the kept one at once", got)
	}
	testutil.Eventually(t, "the background refresh", func() bool { return mustToken(t, s) == "fake-token-2" })
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("Entra was asked %d times, want 2", n)
	}

	// A token past its stale margin is not given: the caller waits for the fetch.
	clock.advance(3599*time.Second - tokenStaleMargin + time.Second)
	if got := mustToken(t, s); got != "fake-token-3" {
		t.Fatalf("token past the stale margin = %q, want a fresh one", got)
	}
}

// Callers that need a token at once share one fetch.
func TestTokenConcurrentCallersShareOneFetch(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	f.gate = make(chan struct{})
	s := f.source(clock)
	const callers = 20
	var wg sync.WaitGroup
	got := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := s.token(t.Context())
			got[i], errs[i] = tok.Reveal(), err
		}()
	}
	testutil.Eventually(t, "a fetch under way", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inflight != nil
	})
	close(f.gate)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || got[i] != "fake-token-1" {
			t.Fatalf("caller %d: %q, %v", i, got[i], errs[i])
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("Entra was asked %d times, want 1", n)
	}
}

// A caller whose context ends stops waiting, and the fetch goes on for the others.
func TestTokenCallerContextDoesNotEndTheFetch(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	f.gate = make(chan struct{})
	s := f.source(clock)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := s.token(ctx); done <- err }()
	testutil.Eventually(t, "a fetch under way", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inflight != nil
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v", err)
	}
	close(f.gate)
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token = %q, want the one the first fetch got", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("Entra was asked %d times, want 1", n)
	}
}

// A refusal of the credentials is a TokenRequestError with Entra's codes and nothing it described; the next fetch waits
// tokenRetryWait, and callers are given the failure until then.
func TestTokenRefusal(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	f.failWith(func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. ` + testClientSecret + `","error_codes":[7000215]}`))
		return true
	})
	s := f.source(clock)
	_, err := s.token(t.Context())
	var te *TokenRequestError
	if !errors.As(err, &te) || te.Status != http.StatusUnauthorized || te.Code != "invalid_client" || te.AADSTS != 7000215 || !te.rejected() {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(err.Error(), testClientSecret) || strings.Contains(err.Error(), "Invalid client") {
		t.Fatalf("the error carries Entra's description: %v", err)
	}
	if _, err := s.token(t.Context()); !errors.As(err, &te) {
		t.Fatalf("within the retry wait: %v, want the failure again", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("Entra was asked %d times within the retry wait, want 1", n)
	}
	f.failWith(nil)
	clock.advance(tokenRetryWait)
	if got := mustToken(t, s); got != "fake-token-2" {
		t.Fatalf("token after the wait = %q", got)
	}
}

// A refresh that fails leaves the kept token in use until it is stale.
func TestTokenFailedRefreshKeepsTheToken(t *testing.T) {
	t.Parallel()
	f, clock := newFakeEntra(t), newFakeClock()
	s := f.source(clock)
	mustToken(t, s)
	f.failWith(func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusServiceUnavailable); return true })
	clock.advance(3599*time.Second - tokenRefreshMargin + time.Second)
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token = %q", got)
	}
	testutil.Eventually(t, "the failed refresh", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.failed != nil
	})
	if got := mustToken(t, s); got != "fake-token-1" {
		t.Fatalf("token after a failed refresh = %q, want the kept one", got)
	}
	clock.advance(tokenRefreshMargin)
	if _, err := s.token(t.Context()); err == nil {
		t.Fatal("a stale token was given after its refreshes failed")
	}
}

// The answers that are not a token are errors, and none of them holds what Entra wrote.
func TestTokenMalformedAnswers(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not json":   `nope`,
		"no token":   `{"token_type":"Bearer","expires_in":3599}`,
		"wrong type": `{"token_type":"pop","expires_in":3599,"access_token":"fake-token"}`,
		"no life":    `{"token_type":"Bearer","expires_in":0,"access_token":"fake-token"}`,
		"too long":   `{"token_type":"Bearer","expires_in":3599,"access_token":"` + strings.Repeat("x", tokenMaxBody) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, clock := newFakeEntra(t), newFakeClock()
			f.failWith(func(w http.ResponseWriter) bool { _, _ = w.Write([]byte(body)); return true })
			if _, err := f.source(clock).token(t.Context()); err == nil || strings.Contains(err.Error(), "fake-token") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A redirect is not followed: the secret goes to the token URL alone.
func TestTokenDoesNotFollowARedirect(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	f, clock := newFakeEntra(t), newFakeClock()
	f.failWith(func(w http.ResponseWriter) bool {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	})
	var te *TokenRequestError
	if _, err := f.source(clock).token(t.Context()); !errors.As(err, &te) || te.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"soon":                          0,
		"Wed, 18 Nov 2026 01:00:30 GMT": 30 * time.Second,
		"Wed, 18 Nov 2026 00:59:30 GMT": 0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestDefaultTokenURL(t *testing.T) {
	t.Parallel()
	if got, want := DefaultTokenURL(testTenant), "https://login.microsoftonline.com/"+testTenant+"/oauth2/v2.0/token"; got != want {
		t.Fatalf("DefaultTokenURL = %q, want %q", got, want)
	}
}
