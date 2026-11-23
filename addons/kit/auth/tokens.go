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
)

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

// TokenVerifier accepts a request that carries one of Tokens in Header, as Caller.
type TokenVerifier struct {
	// Header carries the token, in canonical form. Authorization carries "Bearer <token>" (the scheme in any case);
	// any other header carries the token as its whole value.
	Header string
	// Tokens are the accepted tokens.
	Tokens *Tokens
}

// Verify implements Verifier.
func (v TokenVerifier) Verify(r *http.Request) (string, error) {
	value := r.Header.Get(v.Header)
	if value == "" {
		return "", ErrMissing
	}
	token := value
	if strings.EqualFold(v.Header, "Authorization") {
		var err error
		if token, err = Bearer(value); err != nil {
			return "", err
		}
	}
	if !v.Tokens.Accepts(token) {
		return "", ErrInvalid
	}
	return Caller, nil
}
