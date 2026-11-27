package launcher

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

const maxCallbackTokenSize = 4096

type callbackTokens struct {
	path string
	a2a  *auth.Tokens
	set  atomic.Pointer[[][sha256.Size]byte]
}

func (t *callbackTokens) reload() error {
	file, err := os.Open(t.path)
	if err != nil {
		return errors.New("cannot open AX_RESULT_TOKEN_FILE")
	}
	defer func() { _ = file.Close() }()
	const maxFileSize = 2*maxCallbackTokenSize + 2
	body, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return errors.New("cannot read AX_RESULT_TOKEN_FILE")
	}
	tokens := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	invalid := errors.New("AX_RESULT_TOKEN_FILE must contain one or two distinct callback bearers, one per line")
	if len(body) > maxFileSize || len(tokens) > 2 || len(tokens) == 2 && tokens[0] == tokens[1] {
		return invalid
	}
	digests := make([][sha256.Size]byte, 0, len(tokens))
	for _, token := range tokens {
		if token == "" || len(token) > maxCallbackTokenSize || token == "ax-placeholder" ||
			strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' || r == ',' }) {
			return invalid
		}
		if t.a2a != nil && t.a2a.Accepts(token) {
			return errors.New("AX callback and A2A credentials must differ")
		}
		digests = append(digests, sha256.Sum256([]byte(token)))
	}
	t.set.Store(&digests)
	return nil
}

func (t *callbackTokens) accepts(bearer string) bool {
	if t == nil {
		return false
	}
	// The A2A set can reload independently. An overlap introduced between
	// refreshes must not authorize the token on both interfaces.
	if t.a2a != nil && t.a2a.Accepts(bearer) {
		return false
	}
	got := sha256.Sum256([]byte(bearer))
	match := 0
	if set := t.set.Load(); set != nil {
		for _, want := range *set {
			match |= subtle.ConstantTimeCompare(got[:], want[:])
		}
	}
	return match == 1
}

func (t *callbackTokens) watch(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(auth.ReloadInterval)
	defer tick.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := t.reload(); err != nil {
				if last != err.Error() {
					log.WarnContext(ctx, "could not reload AX callback credentials", "error", err)
					last = err.Error()
				}
			} else {
				last = ""
			}
		}
	}
}
