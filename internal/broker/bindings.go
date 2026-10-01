// Package broker composes courier's parts into the running broker (WP-35). This file holds the
// Bindings the session manager resolves each Binding's backend through.
package broker

import (
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// Snapshots is where the current config comes from: *config.Reloader.
type Snapshots interface {
	// Current returns the latest good snapshot, or nil before the first load.
	Current() *config.Snapshot
}

// Options configures Bindings.
type Options struct {
	// Agent applies to every agent the pools hand out.
	Agent agent.Config
	// ResponseTimeout is the kagent-v1 dialect's bound on a call's response headers; zero means
	// the dialect's default.
	ResponseTimeout time.Duration
	// OnRejected is called once for each credential a backend rejected, for a metric, with the
	// Binding's namespace/name. Turns run concurrently, so it may be called from several
	// goroutines at once and must be safe for that.
	OnRejected func(binding string)
}

// kit is what one dialect needs: the pool that connects through it, its attribution, and how to
// close the idle connections of the transport its clients share.
type kit struct {
	pool      *agent.Pool
	attribute func(*a2a.Message, dialect.User)
	closeIdle func()
}

var _ session.Bindings = (*Bindings)(nil)

// Bindings is a session.Bindings over the config's current snapshot. Every call looks the Binding
// up in the snapshot that is current then, so a reload takes effect on the next call, and builds
// the Binding's identity afresh. The credentials are read for each request the agent makes, never
// kept, so a token rotated in its Secret is sent from the next call on, even by a Backend a
// session already holds. It is safe for concurrent use.
type Bindings struct {
	snaps   Snapshots
	secrets identity.SecretReader
	aud     identity.Auditor
	opts    Options
	kits    map[v1alpha1.Dialect]kit // one pool per dialect; kagent-v1 is the only one built
}

// NewBindings returns Bindings that read the config from snaps, Secrets through secrets, and
// record rejected credentials through aud. Close it when done.
func NewBindings(snaps Snapshots, secrets identity.SecretReader, aud identity.Auditor, opts Options) *Bindings {
	d := kagentv1.New(opts.ResponseTimeout)
	return &Bindings{
		snaps: snaps, secrets: secrets, aud: aud, opts: opts,
		kits: map[v1alpha1.Dialect]kit{v1alpha1.DialectKagentV1: {agent.NewPool(d, opts.Agent), d.Attribute, d.CloseIdleConnections}},
	}
}

// Close closes the agent pools, then the idle connections of the dialects' transports, so that
// no goroutine outlives it. Calls after it fail.
func (b *Bindings) Close() {
	for _, k := range b.kits {
		k.pool.Close()
		k.closeIdle()
	}
}

// Backend returns the backend of the Binding named "namespace/name". An error wrapping
// session.ErrUnknownBinding means the config has no such Binding. Any other error means the
// Binding cannot be served now: no config has loaded yet, or its backend speaks a dialect
// courier has not built.
func (b *Bindings) Backend(name string) (session.Backend, error) {
	snap := b.snaps.Current()
	if snap == nil {
		return session.Backend{}, fmt.Errorf("broker: Binding %s: the config has not loaded yet", name)
	}
	ns, n, ok := strings.Cut(name, "/")
	rb, found := snap.Binding(ns, n)
	if !ok || !found {
		return session.Backend{}, fmt.Errorf("%w: %s", session.ErrUnknownBinding, name)
	}
	k, ok := b.kits[rb.Backend.Spec.Dialect]
	if !ok {
		return session.Backend{}, fmt.Errorf("broker: Binding %s: backend %s uses the %q dialect, and only %q is supported",
			name, v1alpha1.NamespacedName(rb.Backend.Namespace, rb.Backend.Name), rb.Backend.Spec.Dialect, v1alpha1.DialectKagentV1)
	}
	src, err := identity.New(rb.Binding, rb.Backend, b.secrets, b.aud, identity.Options{OnRejected: b.opts.OnRejected})
	if err != nil {
		return session.Backend{}, fmt.Errorf("broker: Binding %s: %w", name, err)
	}
	return session.Backend{
		ThreadTTL: rb.Binding.Spec.Session.ThreadTTL.Duration,
		Agent:     k.pool.Agent(src.Target()),
		Attribute: k.attribute,
		Rejected:  src.Rejected,
		Identity:  src.Identity(),
	}, nil
}
