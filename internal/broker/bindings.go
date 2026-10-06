// Package broker composes courier's parts into the running broker (WP-35). This file holds the
// Bindings the session manager resolves each Binding's backend through.
package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// Snapshots is where the current config comes from: *config.Reloader.
type Snapshots interface {
	// Current returns the latest good snapshot, or nil before the first load.
	Current() *config.Snapshot
}

// Options configures Bindings. OnRejected is required: NewBindings refuses Options without it, and names it. The rest
// are tunables with defaults.
type Options struct {
	// Agent applies to every agent the pools hand out.
	Agent agent.Config
	// ResponseTimeout is the kagent-v1 dialect's bound on a call's response headers; zero means
	// the dialect's default.
	ResponseTimeout time.Duration
	// OnRejected is called once for each credential a backend rejected, for a metric, with the
	// Binding's namespace/name. Turns run concurrently, so it may be called from several
	// goroutines at once and must be safe for that. Required.
	OnRejected func(binding string)
}

// kit is what one dialect needs: the dialect itself, the pool that connects through it, its
// attribution, and how to close the idle connections of the transport its clients share.
type kit struct {
	dialect   dialect.Dialect
	pool      *agent.Pool
	attribute func(*a2a.Message, dialect.User)
	closeIdle func()
}

var (
	_ session.Bindings = (*Bindings)(nil)
	_ interact.Lookup  = (*Bindings)(nil)
)

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
	kits    map[v1alpha1.Dialect]kit // one pool per dialect; a2a and kagent-v1
}

// NewBindings returns Bindings that read the config from snaps, Secrets through secrets, and
// record rejected credentials through aud. It refuses a nil snaps, secrets or aud, and Options without OnRejected,
// naming what is missing. Close it when done.
func NewBindings(snaps Snapshots, secrets identity.SecretReader, aud identity.Auditor, opts Options) (*Bindings, error) {
	switch {
	case snaps == nil:
		return nil, errors.New("broker: snaps is required for Bindings: it is where the config comes from")
	case secrets == nil:
		return nil, errors.New("broker: secrets is required for Bindings: it is where the credentials come from")
	case aud == nil:
		return nil, errors.New("broker: aud is required for Bindings: a rejected credential is recorded")
	case opts.OnRejected == nil:
		return nil, errors.New("broker: Options.OnRejected is required for Bindings: a rejected credential is counted")
	}
	d := kagentv1.New(opts.ResponseTimeout)
	g := generic.New(opts.ResponseTimeout)
	return &Bindings{
		snaps: snaps, secrets: secrets, aud: aud, opts: opts,
		kits: map[v1alpha1.Dialect]kit{v1alpha1.DialectKagentV1: {d, agent.NewPool(d, opts.Agent), d.Attribute, d.CloseIdleConnections}, v1alpha1.DialectA2A: {g, agent.NewPool(g, opts.Agent), g.Attribute, g.CloseIdleConnections}},
	}, nil
}

// Close closes the agent pools, then the idle connections of the dialects' transports, so that
// no goroutine outlives it. Calls after it fail.
func (b *Bindings) Close() {
	for _, k := range b.kits {
		k.pool.Close()
		k.closeIdle()
	}
}

// resolve finds the Binding named "namespace/name" in the current snapshot, and the kit of its
// backend's dialect. The errors are those of Backend.
func (b *Bindings) resolve(name string) (config.ResolvedBinding, kit, error) {
	snap := b.snaps.Current()
	if snap == nil {
		return config.ResolvedBinding{}, kit{}, fmt.Errorf("broker: Binding %s: the config has not loaded yet", name)
	}
	ns, n, ok := strings.Cut(name, "/")
	rb, found := snap.Binding(ns, n)
	if !ok || !found {
		return config.ResolvedBinding{}, kit{}, fmt.Errorf("%w: %s", session.ErrUnknownBinding, name)
	}
	k, ok := b.kits[rb.Backend.Spec.Dialect]
	if !ok {
		return config.ResolvedBinding{}, kit{}, fmt.Errorf("broker: Binding %s: backend %s uses the %q dialect, supported dialects are %q and %q",
			name, v1alpha1.NamespacedName(rb.Backend.Namespace, rb.Backend.Name), rb.Backend.Spec.Dialect, v1alpha1.DialectKagentV1, v1alpha1.DialectA2A)
	}
	return rb, k, nil
}

// Backend returns the backend of the Binding named "namespace/name". An error wrapping
// session.ErrUnknownBinding means the config has no such Binding. Any other error means the
// Binding cannot be served now: no config has loaded yet, or its backend speaks a dialect
// courier has not built.
func (b *Bindings) Backend(name string) (session.Backend, error) {
	rb, k, err := b.resolve(name)
	if err != nil {
		return session.Backend{}, err
	}
	src, err := identity.New(rb.Binding, rb.Backend, b.secrets, b.aud, identity.Options{OnRejected: b.opts.OnRejected})
	if err != nil {
		return session.Backend{}, fmt.Errorf("broker: Binding %s: %w", name, err)
	}
	return session.Backend{
		Name: v1alpha1.NamespacedName(rb.Backend.Namespace, rb.Backend.Name), AgentName: v1alpha1.NamespacedName(rb.Binding.Spec.Agent.Namespace, rb.Binding.Spec.Agent.Name), Dialect: string(rb.Backend.Spec.Dialect),
		ThreadTTL:     rb.Binding.Spec.Session.ThreadTTL.Duration,
		Agent:         k.pool.Agent(src.Target()),
		Attribute:     k.attribute,
		Rejected:      src.Rejected,
		Identity:      src.Identity(),
		Output:        rb.Binding.Spec.Chat.Output,
		TextReplies:   rb.Backend.Spec.Dialect == v1alpha1.DialectA2A,
		Feedback:      rb.Binding.Spec.Feedback.Enabled,
		ThreadReplies: rb.Binding.Spec.Chat.ThreadReplies,
	}, nil
}

// CancelTask asks the agent of the Binding named "namespace/name" to cancel the task, for the retention of a task that a
// crash left on a session that expired long ago, and returns the Binding's service identity, which the call was made under
// (it is empty when the Binding could not be resolved, and nothing was called). An error wrapping session.ErrUnknownBinding
// means the config has no such Binding; any other is the agent's, or a Binding that cannot be served now.
func (b *Bindings) CancelTask(ctx context.Context, binding, taskID string) (string, error) {
	be, err := b.Backend(binding)
	if err != nil {
		return "", err
	}
	if _, err := be.Agent.CancelTask(ctx, a2a.TaskID(taskID)); err != nil {
		return be.Identity, fmt.Errorf("cancel task: %w", err)
	}
	return be.Identity, nil
}

// ThreadTTLs returns the threadTTL of every Binding of the current snapshot, by "namespace/name", and false before the
// first load, when nothing is known: it is what the retention of sessions measures a session's expiry by.
func (b *Bindings) ThreadTTLs() (map[string]time.Duration, bool) {
	snap := b.snaps.Current()
	if snap == nil {
		return nil, false
	}
	ttls := map[string]time.Duration{}
	for _, rb := range snap.Bindings() {
		ttls[v1alpha1.NamespacedName(rb.Binding.Namespace, rb.Binding.Name)] = cmp.Or(rb.Binding.Spec.Session.ThreadTTL.Duration, v1alpha1.DefaultThreadTTL)
	}
	return ttls, true
}

// Binding returns what the interactions need of the Binding named "namespace/name", from the
// snapshot that is current: the dialect that reads and answers its pauses, the identity its audit
// rows name, which pauses it allows, and how long one waits. Its errors are those of Backend: one
// wrapping session.ErrUnknownBinding means the config has no such Binding.
func (b *Bindings) Binding(name string) (interact.Binding, error) {
	rb, k, err := b.resolve(name)
	if err != nil {
		return interact.Binding{}, err
	}
	src, err := identity.New(rb.Binding, rb.Backend, b.secrets, b.aud, identity.Options{})
	if err != nil {
		return interact.Binding{}, fmt.Errorf("broker: Binding %s: %w", name, err)
	}
	in := rb.Binding.Spec.Interactions
	return interact.Binding{
		Dialect: k.dialect, Identity: src.Identity(), AskUser: in.AskUser, ToolApprovals: in.ToolApprovals, Timeout: in.Timeout.Duration,
	}, nil
}
