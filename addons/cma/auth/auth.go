// Package auth is the adapter's caller check: a request to the A2A endpoint must carry one of the tokens in a file,
// which a mounted Secret keeps current. Every accepted token is the same caller, Agent Kourier: tokens exist so that
// one can be rotated in before the other is removed, not to tell callers apart.
package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Caller is the user name a checked request runs as. The task store records it as each task's owner; tasks are not
// partitioned by it, since every accepted token is Agent Kourier.
const Caller = "agent-kourier"

// ReloadInterval is how often Watch re-reads the token file.
const ReloadInterval = 5 * time.Second

// Tokens is the set of accepted tokens, read from a file: one token per line, with blank lines and lines that start
// with # left out and trailing whitespace (a CR included) stripped. Only each token's SHA-256 is kept.
type Tokens struct {
	path string
	log  *slog.Logger

	mu      sync.RWMutex
	digests [][sha256.Size]byte
	content []byte // the file as last read, to tell a change from a re-read
	failing string // the reload error last logged, so a failure is logged once until it changes
}

// LoadTokens reads the tokens in path. It fails when the file cannot be read or holds no token.
func LoadTokens(path string, log *slog.Logger) (*Tokens, error) {
	t := &Tokens{path: path, log: log}
	content, digests, err := read(path)
	if err != nil {
		return nil, err
	}
	t.content, t.digests = content, digests
	return t, nil
}

func read(path string) ([]byte, [][sha256.Size]byte, error) {
	content, err := os.ReadFile(path) //nolint:gosec // the path is the operator's AUTH_TOKEN_FILE
	if err != nil {
		return nil, nil, fmt.Errorf("read the token file: %w", err)
	}
	var digests [][sha256.Size]byte
	for _, line := range strings.Split(string(content), "\n") {
		token := strings.TrimRight(line, " \t\r")
		if trimmed := strings.TrimSpace(token); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		digests = append(digests, sha256.Sum256([]byte(token)))
	}
	if len(digests) == 0 {
		return nil, nil, errors.New("the token file holds no token")
	}
	return content, digests, nil
}

// Watch re-reads the file every interval until ctx ends. A mounted Secret changes by swapping a symlink, so the file
// is read again rather than watched for events. A read that fails or finds no token keeps the last good set, and is
// logged once until the failure changes.
func (t *Tokens) Watch(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.reload()
		}
	}
}

func (t *Tokens) reload() {
	content, digests, err := read(t.path)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		if err.Error() != t.failing {
			t.failing = err.Error()
			t.log.Warn("reload the accepted tokens; keeping the last good tokens", "file", t.path, "error", err)
		}
		return
	}
	if t.failing != "" {
		t.failing = ""
		t.log.Info("the token file is readable again", "file", t.path)
	}
	if bytes.Equal(content, t.content) {
		return
	}
	t.content, t.digests = content, digests
	t.log.Info("reloaded the accepted tokens", "file", t.path, "tokens", len(digests))
}

// Accepts reports whether token is one of the set. It compares digests in constant time, against every token, so
// the time it takes does not say which token matched or how much of one did.
func (t *Tokens) Accepts(token string) bool {
	got := sha256.Sum256([]byte(token))
	t.mu.RLock()
	defer t.mu.RUnlock()
	match := 0
	for _, want := range t.digests {
		match |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return match == 1
}

// Check refuses a request that does not carry an accepted token.
type Check struct {
	// Header carries the token, in canonical form. Authorization carries "Bearer <token>" (the scheme in any case);
	// any other header carries the token as its whole value.
	Header string
	// Tokens are the accepted tokens.
	Tokens *Tokens
	// Log receives each refusal, with its reason and never the presented value.
	Log *slog.Logger
}

// callerKey marks a request context that passed the check.
type callerKey struct{}

// Wrap returns next behind the check. A refused request gets 401, with a Bearer challenge when the header is
// Authorization.
func (c *Check) Wrap(next http.Handler) http.Handler {
	bearer := strings.EqualFold(c.Header, "Authorization")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, reason := c.token(r, bearer)
		if reason == "" && !c.Tokens.Accepts(token) {
			reason = "invalid"
		}
		if reason != "" {
			c.Log.Warn("refused an A2A request", "reason", reason, "header", c.Header, "remote", r.RemoteAddr)
			if bearer {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, true)))
	})
}

// token returns the presented token, or the reason there is none: missing, or invalid for an Authorization header
// that is not a Bearer one.
func (c *Check) token(r *http.Request, bearer bool) (token, reason string) {
	v := r.Header.Get(c.Header)
	if v == "" {
		return "", "missing"
	}
	if !bearer {
		return v, ""
	}
	scheme, token, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", "invalid"
	}
	return token, ""
}

// Interceptor makes a call that passed the check run as Caller, so that the task store records an owner. A call that
// did not pass it (the check is off) is left unauthenticated.
type Interceptor struct {
	a2asrv.PassthroughCallInterceptor
}

// Before implements a2asrv.CallInterceptor.
func (Interceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	if ok, _ := ctx.Value(callerKey{}).(bool); ok {
		callCtx.User = a2asrv.NewAuthenticatedUser(Caller, nil)
	}
	return ctx, nil, nil
}
