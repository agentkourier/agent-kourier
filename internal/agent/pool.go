package agent

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

var (
	// ErrStreamingUnsupported means the agent's card does not advertise capabilities.streaming.
	// a2a-go would then fall back to a unary SendMessage, whose response headers arrive only
	// when the task ends, and the dialect's response-header timeout (30 s) would cut every
	// long task. Agent Kourier refuses such an agent instead.
	ErrStreamingUnsupported = errors.New("agent: the agent card does not advertise streaming")
	// ErrTokenRequired means a Target with TokenRequired got credentials without a token.
	ErrTokenRequired = errors.New("agent: the Binding requires a bearer token and the credentials carry none")
	// ErrTokenUnreadable means the Binding's token could not be read, or read as an empty or
	// malformed value. The turn fails: a Binding with a token never falls back to its userId.
	// The error wraps the cause.
	ErrTokenUnreadable = errors.New("agent: the Binding's token cannot be read")
	// ErrHeaderUnreadable means the Secret behind a static header could not be read. The turn
	// fails: the backend's front door would refuse it, or worse, treat it as anonymous.
	ErrHeaderUnreadable = errors.New("agent: a static header's secret cannot be read")
	// ErrPoolClosed is returned by a call made after Pool.Close.
	ErrPoolClosed = errors.New("agent: pool is closed")
)

// CredentialsFunc reads the Binding's current credentials, and fails when a configured token
// cannot be read. The Pool calls it for every operation and stores nothing it returns, so a
// rotated token (WP-21e) takes effect on the next call. On error the call fails: the
// identity never falls back to X-User-Id.
//
// It must return when ctx ends. Pool.Check passes a context that ends when the Pool closes or
// ConnectTimeout passes, and Pool.Close waits for the Check, so a function that ignores ctx holds Close.
type CredentialsFunc func(ctx context.Context) (dialect.Credentials, error)

// Target is one agent as one Binding reaches it.
type Target struct {
	Binding     string // the Binding's namespace/name; with Agent, the cache key
	Backend     *v1alpha1.AgentBackend
	Agent       v1alpha1.AgentRef
	Credentials CredentialsFunc
	// TokenRequired is set when the Binding names a token secret (identity.tokenSecretRef).
	// The Pool then refuses credentials with an empty Token before connecting: defense in
	// depth against a provider that would downgrade to X-User-Id.
	TokenRequired bool
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
// the credentials change. Connecting fetches the agent card, a network round trip, on an
// Agent's first use, so that use belongs off Slack's 3-second acknowledgement path. It is
// safe for concurrent use.
type Pool struct {
	dialect dialect.Dialect
	cfg     Config
	bg      context.Context // ends the connects in flight when the pool closes
	stop    context.CancelFunc
	key     [32]byte // keys the seal

	mu      sync.Mutex
	entries map[poolKey]*entry
	closed  bool
	probes  sync.WaitGroup // the Checks in flight; Add only under mu while not closed, so Close's Wait cannot race it
}

// NewPool returns a Pool that connects through d. cfg applies to every Agent it returns.
func NewPool(d dialect.Dialect, cfg Config) *Pool {
	bg, stop := context.WithCancel(context.Background())
	p := &Pool{dialect: d, cfg: cfg.withDefaults(), bg: bg, stop: stop, entries: map[poolKey]*entry{}}
	_, _ = rand.Read(p.key[:]) // never fails: Go crashes the program if the system has no randomness
	return p
}

// Agent returns an Agent for t. It is cheap and connects on first use.
func (p *Pool) Agent(t Target) *Agent {
	cfg := p.cfg
	cfg.NotReady = p.dialect.NotReady
	cfg.ParseInteraction = p.dialect.ParseInteraction
	return New(func(ctx context.Context) (Conn, error) { return p.conn(ctx, t) }, cfg)
}

// Close cancels the connects and the Checks in flight, waits for the Checks to return, and closes every
// client. Calls after it fail with ErrPoolClosed; streams already running end with their own context.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	entries := slices.Collect(maps.Values(p.entries))
	p.mu.Unlock()
	p.stop()
	p.probes.Wait() // after the lock is released: a Check takes it to finish
	for _, e := range entries {
		e.mu.Lock()
		if e.client != nil {
			_ = e.client.Destroy()
			e.client = nil
		}
		e.mu.Unlock()
	}
}

// credentials resolves t's credentials, and refuses a Target that requires a token when they carry none.
func (p *Pool) credentials(ctx context.Context, t Target) (dialect.Credentials, error) {
	creds, err := t.Credentials(ctx)
	if err != nil {
		return dialect.Credentials{}, fmt.Errorf("resolve credentials of binding %s: %w", t.Binding, err)
	}
	if t.TokenRequired && creds.Token.IsEmpty() {
		return dialect.Credentials{}, fmt.Errorf("binding %s: %w", t.Binding, ErrTokenRequired)
	}
	return creds, nil
}

// bounded returns ctx ended also by the ConnectTimeout and by the pool closing, and the function that releases it.
// Close waits for what holds the entry's lock (a connect) and for the Checks, so what they wait on must end with the pool.
func (p *Pool) bounded(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout)
	stop := context.AfterFunc(p.bg, cancel)
	return ctx, func() { stop(); cancel() }
}

// connect makes a client for t with creds, which fetches the agent card, and refuses an agent whose card does
// not advertise streaming. A client it refuses is destroyed. The connect ends when the caller's context, the
// ConnectTimeout, or the pool ends.
func (p *Pool) connect(ctx context.Context, t Target, creds dialect.Credentials) (*a2aclient.Client, error) {
	ctx, release := p.bounded(ctx)
	defer release()
	return p.open(ctx, t, creds)
}

// open is connect without the bound: ctx must already end with the pool.
func (p *Pool) open(ctx context.Context, t Target, creds dialect.Credentials) (*a2aclient.Client, error) {
	client, err := p.dialect.Connect(ctx, t.Backend, t.Agent, creds)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", t.Binding, err)
	}
	// WP-01d measured kagent 1.0.0-alpha5's card: capabilities.streaming is true
	// (test/fixtures/kagent/card/test-agent.card.json).
	if !client.Card().Capabilities.Streaming {
		_ = client.Destroy()
		return nil, fmt.Errorf("agent %s/%s: %w", t.Agent.Namespace, t.Agent.Name, ErrStreamingUnsupported)
	}
	return client, nil
}

// conn resolves the credentials, then returns the cached client if they and the backend URL
// are unchanged, or connects a new one.
func (p *Pool) conn(ctx context.Context, t Target) (Conn, error) {
	creds, err := p.credentials(ctx, t)
	if err != nil {
		return nil, err
	}
	e := p.entry(poolKey{t.Binding, t.Agent})
	seal := p.seal(t.Backend.Spec.URL, creds)
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.isClosed() { // Close may have run while this call waited for a connect to finish
		return nil, ErrPoolClosed
	}
	if e.client != nil && e.seal == seal {
		return e.client, nil
	}
	client, err := p.connect(ctx, t, creds)
	if err != nil {
		return nil, err
	}
	e.client, e.seal = client, seal // a replaced client is left to finish the streams it carries
	return client, nil
}

// Check reports whether t's agent can be reached now: it resolves the credentials as a call does,
// connects afresh, which fetches the agent card, applies the check that the card advertises
// streaming, and destroys the client. It sends the agent no message.
//
// The connect is always fresh because conn returns the cached client while the credentials and the
// URL are unchanged, so a probe through it would stop touching the backend after its first success.
// Check neither reads nor writes the cached clients, so it cannot disturb a stream or hide an
// outage behind one. It also asks the dialect for a new connection (dialect.WithFreshConnection): the card
// fetch does its own DNS lookup, dial and TLS handshake, so a connection a turn holds open cannot carry
// it, and a name, certificate or network path that only a new connection needs fails the check.
// It holds no entry lock while it waits on the network, so it never delays a turn.
//
// The credentials read and the connect together end at the ConnectTimeout, when ctx ends, or when the
// Pool closes. It returns the error of the card fetch as it is, not typed by what a status
// means to a call: a 403 is a dialect.HTTPStatusError and not ErrCredentialsRejected. It also
// returns ErrTokenUnreadable, ErrHeaderUnreadable, ErrTokenRequired, ErrStreamingUnsupported,
// dialect.ErrUnsupportedExtension and dialect.ErrNoJSONRPCInterface, all findable with errors.Is.
// After Close it returns ErrPoolClosed: at once when Close came before the call, and otherwise when Close
// cancelled the credentials read or the connect. Close waits for a Check that is in flight, so none outlives it.
// An error that is not a cancellation keeps its own type, even when Close runs just after it.
func (p *Pool) Check(ctx context.Context, t Target) error {
	if !p.beginProbe() {
		return ErrPoolClosed
	}
	defer p.probes.Done()
	ctx, release := p.bounded(dialect.WithFreshConnection(ctx))
	defer release()
	creds, err := p.credentials(ctx, t)
	if err != nil {
		return p.closedOr(err)
	}
	client, err := p.open(ctx, t, creds)
	if err != nil {
		return p.closedOr(err)
	}
	_ = client.Destroy()
	return nil
}

// beginProbe registers a Check unless the pool is closed. It registers under the lock Close sets closed under, so
// Close's wait sees every Check that was not refused.
func (p *Pool) beginProbe() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.probes.Add(1)
	return true
}

// closedOr is ErrPoolClosed for an error that Close's cancellation caused, and err otherwise.
func (p *Pool) closedOr(err error) error {
	if errors.Is(err, context.Canceled) && p.isClosed() {
		return ErrPoolClosed
	}
	return err
}

// entry returns the entry for k, made on first use. It does not look at closed: conn does,
// under the entry's lock, which is the one check that cannot be raced.
func (p *Pool) entry(k poolKey) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[k]
	if !ok {
		e = &entry{}
		p.entries[k] = e
	}
	return e
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// seal digests the backend URL and credentials so the Pool can tell that they changed
// without keeping them: Credentials redacts itself in logs, and a Pool field would not. It
// is an HMAC under a key made in NewPool, so the digest is worthless outside this process,
// and it is never used to authenticate anything. The encoding is canonical: every field is
// length-prefixed, the header names are sorted, and each name is followed by its value count.
func (p *Pool) seal(backendURL string, c dialect.Credentials) [sha256.Size]byte {
	h := hmac.New(sha256.New, p.key[:])
	put := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s", len(s), s) }
	put(backendURL)
	put(c.Token.Reveal())
	put(c.UserID)
	names := slices.Sorted(maps.Keys(c.Headers))
	put(strconv.Itoa(len(names)))
	for _, name := range names {
		put(name)
		put(strconv.Itoa(len(c.Headers[name])))
		for _, v := range c.Headers[name] {
			put(v.Reveal())
		}
	}
	return [sha256.Size]byte(h.Sum(nil))
}
