package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Fake tokens: obviously not real credentials.
const (
	tokenA = "fake-token-alpha"
	tokenB = "fake-token-bravo"
)

// syncBuffer is a log sink the watcher and the test may use at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLog() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseTokens(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		accept        []string
		refuse        []string
	}{
		{"one", tokenA, []string{tokenA}, []string{tokenB, "", tokenA + " "}},
		{"several with comments and blanks", "# current\n" + tokenA + "\n\n   \n# next, for rotation\n" + tokenB + "\n",
			[]string{tokenA, tokenB}, []string{"# current", "# next, for rotation", ""}},
		{"trailing whitespace and CR stripped", tokenA + " \t\r\n" + tokenB + "\r\n", []string{tokenA, tokenB}, []string{tokenA + " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens")
			writeFile(t, path, tc.content)
			log, _ := newLog()
			tokens, err := LoadTokens(path, log)
			if err != nil {
				t.Fatalf("LoadTokens: %v", err)
			}
			for _, tok := range tc.accept {
				if !tokens.Accepts(tok) {
					t.Errorf("refused %q", tok)
				}
			}
			for _, tok := range tc.refuse {
				if tokens.Accepts(tok) {
					t.Errorf("accepted %q", tok)
				}
			}
		})
	}
}

func TestLoadTokensRefuses(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	writeFile(t, empty, "# nothing but a comment\n\n")
	log, _ := newLog()
	for name, path := range map[string]string{"missing file": filepath.Join(dir, "absent"), "no tokens": empty} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadTokens(path, log); err == nil {
				t.Fatal("LoadTokens accepted it")
			}
		})
	}
}

// A mounted Secret changes by a symlink swap: the watcher must see the new content, and keep the last good set when
// the file is unreadable or holds no token.
func TestReloadFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "v1"), tokenA+"\n")
	writeFile(t, filepath.Join(dir, "v2"), tokenB+"\n")
	link := filepath.Join(dir, "tokens")
	if err := os.Symlink(filepath.Join(dir, "v1"), link); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	tokens, err := LoadTokens(link, log)
	if err != nil {
		t.Fatal(err)
	}

	swap := func(target string) {
		t.Helper()
		tmp := filepath.Join(dir, "tokens.tmp")
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
	}

	swap(filepath.Join(dir, "v2"))
	tokens.reload()
	if tokens.Accepts(tokenA) || !tokens.Accepts(tokenB) {
		t.Fatal("after the swap the old token is still accepted or the new one is not")
	}

	writeFile(t, filepath.Join(dir, "v3"), "# emptied by mistake\n")
	swap(filepath.Join(dir, "v3"))
	tokens.reload()
	if !tokens.Accepts(tokenB) {
		t.Fatal("a file with no token replaced the last good set")
	}
	swap(filepath.Join(dir, "gone"))
	tokens.reload()
	tokens.reload() // a second failure in a row is not logged again
	if !tokens.Accepts(tokenB) {
		t.Fatal("an unreadable file replaced the last good set")
	}
	logs := buf.String()
	if n := strings.Count(logs, "keeping the last good tokens"); n != 2 {
		t.Errorf("logged %d failed reloads, want 2 (one per new failure):\n%s", n, logs)
	}
	if strings.Contains(logs, tokenA) || strings.Contains(logs, tokenB) {
		t.Fatalf("the log shows a token:\n%s", logs)
	}
}

func TestWatchReloadsUntilCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	writeFile(t, path, tokenA)
	log, _ := newLog()
	tokens, err := LoadTokens(path, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tokens.Watch(ctx, 5*time.Millisecond); close(done) }()
	writeFile(t, path, tokenB)
	deadline := time.Now().Add(5 * time.Second)
	for !tokens.Accepts(tokenB) {
		if time.Now().After(deadline) {
			t.Fatal("Watch never picked up the new token")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// serve runs one request through the check and reports the status, the challenge and whether the inner handler ran
// as the authenticated caller.
func serve(t *testing.T, header string, set map[string]string) (status int, challenge string, user *a2asrv.User) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	writeFile(t, path, tokenA+"\n"+tokenB+"\n")
	log, buf := newLog()
	tokens, err := LoadTokens(path, log)
	if err != nil {
		t.Fatal(err)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What the JSON-RPC handler does: a fresh call context, then the interceptor.
		ctx, callCtx := a2asrv.NewCallContext(r.Context(), nil)
		if _, _, err := (Interceptor{}).Before(ctx, callCtx, &a2asrv.Request{}); err != nil {
			t.Errorf("Before: %v", err)
		}
		user = callCtx.User
		w.WriteHeader(http.StatusOK)
	})
	h := (&Check{Header: header, Tokens: tokens, Log: log}).Wrap(inner)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/a2a", strings.NewReader(`{}`))
	for k, v := range set {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, v := range set {
		if v != "" && strings.Contains(buf.String()+body, v) {
			t.Errorf("the log or the response shows the presented value %q", v)
		}
	}
	return rec.Code, rec.Header().Get("WWW-Authenticate"), user
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		header    string
		set       map[string]string
		status    int
		challenge string
	}{
		{"bearer", "Authorization", map[string]string{"Authorization": "Bearer " + tokenA}, http.StatusOK, ""},
		{"second token", "Authorization", map[string]string{"Authorization": "Bearer " + tokenB}, http.StatusOK, ""},
		{"scheme in any case", "Authorization", map[string]string{"Authorization": "bEaReR " + tokenA}, http.StatusOK, ""},
		{"missing", "Authorization", nil, http.StatusUnauthorized, "Bearer"},
		{"wrong token", "Authorization", map[string]string{"Authorization": "Bearer fake-token-wrong"}, http.StatusUnauthorized, "Bearer"},
		{"no scheme", "Authorization", map[string]string{"Authorization": tokenA}, http.StatusUnauthorized, "Bearer"},
		{"basic", "Authorization", map[string]string{"Authorization": "Basic " + tokenA}, http.StatusUnauthorized, "Bearer"},
		{"empty bearer", "Authorization", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized, "Bearer"},
		{"a prefix of a token", "Authorization", map[string]string{"Authorization": "Bearer fake-token"}, http.StatusUnauthorized, "Bearer"},
		{"named header", "X-Kourier-Token", map[string]string{"X-Kourier-Token": tokenA}, http.StatusOK, ""},
		{"named header carries the whole value", "X-Kourier-Token", map[string]string{"X-Kourier-Token": "Bearer " + tokenA}, http.StatusUnauthorized, ""},
		{"named header ignores Authorization", "X-Kourier-Token", map[string]string{"Authorization": "Bearer " + tokenA}, http.StatusUnauthorized, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, challenge, user := serve(t, tc.header, tc.set)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if challenge != tc.challenge {
				t.Errorf("WWW-Authenticate = %q, want %q", challenge, tc.challenge)
			}
			if status == http.StatusOK && (user == nil || !user.Authenticated || user.Name != Caller) {
				t.Errorf("call context user = %+v, want the authenticated %s", user, Caller)
			}
		})
	}
}

// A request that did not pass the check (AUTH_DISABLED) has no authenticated user.
func TestInterceptorLeavesAnUncheckedCallAlone(t *testing.T) {
	ctx, callCtx := a2asrv.NewCallContext(context.Background(), nil)
	if _, _, err := (Interceptor{}).Before(ctx, callCtx, &a2asrv.Request{}); err != nil {
		t.Fatal(err)
	}
	if callCtx.User.Authenticated {
		t.Fatalf("user = %+v, want unauthenticated", callCtx.User)
	}
}
