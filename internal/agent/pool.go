package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

var (
	// ErrStreamingUnsupported means the agent's card does not advertise capabilities.streaming.
	// a2a-go would then fall back to a unary SendMessage, whose response headers arrive only
	// when the task ends, and the dialect's response-header timeout (30 s) would cut every
	// long task. Courier refuses such an agent instead.
	ErrStreamingUnsupported = errors.New("agent: the agent card does not advertise streaming")
	// ErrPoolClosed is returned by a call made after Pool.Close.
	ErrPoolClosed = errors.New("agent: pool is closed")
)

// CredentialsFunc reads the Binding's current credentials, and fails when a configured token
// cannot be read. The Pool calls it for every operation and stores nothing it returns, so a
// rotated token (WP-21e) takes effect on the next call. On error the call fails: the
// identity never falls back to X-User-Id.
type CredentialsFunc func(ctx context.Context) (dialect.Credentials, error)

// Target is one agent as one Binding reaches it.
type Target struct {
	Binding     string // the Binding's namespace/name; with Agent, the cache key
	Backend     *v1alpha1.AgentBackend
	Agent       v1alpha1.AgentRef
	Credentials CredentialsFunc
}

type poolKey struct {
	binding string
	agent   v1alpha1.AgentRef
}

type entry struct {
	mu     sync.Mutex // held while connecting, so callers for one key share one connect
	client *a2aclient.Client
	seal   [sha256.Size]byte // digest of what the client was made with; see seal
}

// Pool keeps one connected client per (Binding, agent), reconnecting when the backend URL or
// the credentials change. Connecting fetches the agent card, a network round trip: keep it
// off Slack's 3-second acknowledgement path by calling Warm there and Send later. It is safe
// for concurrent use.
type Pool struct {
	dialect dialect.Dialect
	cfg     Config
	bg      context.Context // ends the warm-ups when the pool closes
	stop    context.CancelFunc
	warming sync.WaitGroup

	mu      sync.Mutex
	entries map[poolKey]*entry
	closed  bool
}

// NewPool returns a Pool that connects through d. cfg applies to every Agent it returns.
func NewPool(d dialect.Dialect, cfg Config) *Pool {
	bg, stop := context.WithCancel(context.Background())
	return &Pool{dialect: d, cfg: cfg.withDefaults(), bg: bg, stop: stop, entries: map[poolKey]*entry{}}
}

// Agent returns an Agent for t. It is cheap and connects on first use.
func (p *Pool) Agent(t Target) *Agent {
	return New(func(ctx context.Context) (Conn, error) { return p.conn(ctx, t) }, p.cfg)
}

// Warm connects to t's agent in the background and returns at once. A failed warm-up is
// forgotten: the next call connects again and reports the error.
func (p *Pool) Warm(t Target) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.warming.Add(1)
	go func() {
		defer p.warming.Done()
		_, _ = p.conn(p.bg, t)
	}()
}

// Close stops the warm-ups, waits for them, and closes every client. Calls after it fail
// with ErrPoolClosed; streams already running end with their own context.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	entries := slices.Collect(maps.Values(p.entries))
	p.mu.Unlock()
	p.stop()
	p.warming.Wait()
	for _, e := range entries {
		e.mu.Lock()
		if e.client != nil {
			_ = e.client.Destroy()
			e.client = nil
		}
		e.mu.Unlock()
	}
}

// conn resolves the credentials, then returns the cached client if they and the backend URL
// are unchanged, or connects a new one.
func (p *Pool) conn(ctx context.Context, t Target) (Conn, error) {
	creds, err := t.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials of binding %s: %w", t.Binding, err)
	}
	e, err := p.entry(poolKey{t.Binding, t.Agent})
	if err != nil {
		return nil, err
	}
	seal := seal(t.Backend.Spec.URL, creds)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil && e.seal == seal {
		return e.client, nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout)
	defer cancel()
	client, err := p.dialect.Connect(ctx, t.Backend, t.Agent, creds)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", t.Binding, err)
	}
	if !client.Card().Capabilities.Streaming {
		return nil, fmt.Errorf("agent %s/%s: %w", t.Agent.Namespace, t.Agent.Name, ErrStreamingUnsupported)
	}
	e.client, e.seal = client, seal // a replaced client is left to finish the streams it carries
	return client, nil
}

func (p *Pool) entry(k poolKey) (*entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	e, ok := p.entries[k]
	if !ok {
		e = &entry{}
		p.entries[k] = e
	}
	return e, nil
}

// seal digests the backend URL and credentials so the Pool can tell that they changed
// without keeping them: Credentials redacts itself in logs, and a Pool field would not.
// The digest is not reversible and is never used to authenticate anything.
func seal(backendURL string, c dialect.Credentials) [sha256.Size]byte {
	h := sha256.New()
	for _, s := range []string{backendURL, c.Token, c.UserID, fmt.Sprint(c.Headers)} { // fmt sorts map keys
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s) // length-prefixed, so fields cannot run together
	}
	return [sha256.Size]byte(h.Sum(nil))
}
