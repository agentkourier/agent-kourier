package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// Stub drives the in-cluster stub agent's control surface, so a scenario can release the task that
// scenario-gated holds. The surface has no authentication, so this client is only for tests inside
// the cluster, and the stub's Service is never published.
type Stub struct {
	c   *client
	run string        // identifies this client, so names from another run never collide
	seq atomic.Uint64 // counts the names made
}

// NewStub returns a client for the stub agent at baseURL.
func NewStub(baseURL string) *Stub {
	b := make([]byte, 4)
	run := strconv.FormatInt(time.Now().UnixNano(), 36) // only if the system has no randomness
	if _, err := rand.Read(b); err == nil {
		run = hex.EncodeToString(b)
	}
	return &Stub{c: newClient(baseURL), run: run}
}

var notGateRune = regexp.MustCompile(`[^\w-]+`)

// Gate returns a gate name for a task of the test called name (t.Name()): the run, the name with
// anything a gate cannot hold replaced, and a count. The stub keeps a gate open for the rest of its
// life, so a name used twice would already be released: use each once, in the prompt that holds
// the task ("gate:" plus the name) and in the Release that frees it.
func (s *Stub) Gate(name string) string {
	return fmt.Sprintf("%s-%s-%d", s.run, notGateRune.ReplaceAllString(name, "-"), s.seq.Add(1))
}

// Release opens gate: the task held on it goes on, and one that reaches it later does not wait.
// It refuses a name the stub could not take, such as one not made by Gate.
func (s *Stub) Release(ctx context.Context, gate string) error {
	if !stubagent.ValidGate(gate) {
		return fmt.Errorf("e2e: %q is not a valid gate name; make one with Gate", gate)
	}
	return s.c.do(ctx, http.MethodPost, "/_control/release/"+gate, nil, nil)
}
