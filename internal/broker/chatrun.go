package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

var (
	// ErrSlackAuth is wrapped by the error RunPlatform returns when the chat platform rejected the credentials for good
	// (Slack, in v1, which the name keeps for the exit code that cmd/agent-kourier maps it to):
	// the process exits, with the code in the error, because no retry would help.
	ErrSlackAuth = errors.New("slack rejected the tokens")
	// ErrWiring is wrapped by an error that means the broker was put together wrongly, or its
	// config is invalid or asks for what v1 does not do: a usage error, not a failure of a dependency.
	ErrWiring = errors.New("the broker cannot be wired")
)

// PlatformRunner is a chat platform as RunPlatform runs it: chat.Platform. Run holds the connection until it fails or
// ctx ends, and Classify says which of its errors no retry cures (chat.FatalAuth).
type PlatformRunner interface {
	Run(ctx context.Context, sink chat.Sink) error
	Classify(op chat.Op, err error) chat.Verdict
}

// RunPolicy is how RunPlatform paces its retries. Zero fields take their defaults.
type RunPolicy struct {
	// Base is the first pause after a failure, doubling up to Max. Defaults: 1 s and 60 s.
	Base, Max time.Duration
	// ResetAfter is how long a connection must have been up for the next failure to start the
	// backoff again from Base. Default 120 s.
	ResetAfter time.Duration
	// Now and After are the clock; a test injects its own. Defaults: time.Now and time.After.
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time
	Logger *slog.Logger // slog.Default() if nil
}

// RunPlatform runs the chat connection for as long as ctx lives. An adapter reconnects by itself, so
// when Run returns, the connection failed for good or never opened. An error that the platform classifies as
// credentials it rejects (chat.FatalAuth) ends RunPlatform at once with ErrSlackAuth, since no retry cures
// it. Any other error is retried after a pause that doubles from Base to Max, and starts over from
// Base once a run has lasted ResetAfter. It returns an error wrapping ctx's when ctx ends: a
// shutdown, not a failure. The errors it logs come from Run, which keeps tokens out of them.
func RunPlatform(ctx context.Context, runner PlatformRunner, sink chat.Sink, p RunPolicy) error {
	base, limit, reset := cmp.Or(p.Base, time.Second), cmp.Or(p.Max, time.Minute), cmp.Or(p.ResetAfter, 120*time.Second)
	now, after, log := p.Now, p.After, p.Logger
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	if log == nil {
		log = slog.Default()
	}
	pause := min(base, limit)
	for {
		started := now()
		err := runner.Run(ctx, sink)
		if ctx.Err() != nil {
			return stopped(ctx)
		}
		if v := runner.Classify(chat.OpOther, err); v.Class == chat.FatalAuth {
			return fmt.Errorf("%w: %s", ErrSlackAuth, v.Code)
		}
		if now().Sub(started) >= reset {
			pause = min(base, limit)
		}
		log.WarnContext(ctx, "slack connection ended; connecting again", "error", err, "after", pause)
		select {
		case <-ctx.Done():
			return stopped(ctx)
		case <-after(pause):
		}
		pause = min(pause*2, limit)
	}
}

// ChooseConnection returns the ChatConnection this broker runs, with the keys of its credentials Secret that its
// platform needs (platformKeys) read through secrets. With the custom resources own is the broker's own connection
// (RunConfig.ChatConnection), found by its reference: the snapshot may hold the ChatConnections of the admitted namespaces
// besides it, and those are not run yet, so their Secrets are not read here and none of them can fail this. With the files
// own is the zero value, and the directory must hold exactly one ChatConnection, as the broker serves one. The connection must be on a platform that has
// an adapter (Slack, in Socket Mode). Anything else is an error wrapping ErrWiring. The tokens are those of the snapshot at the call: Run reads them at start, and again when the
// leader half starts, since a standby may lead long after it started. A change of the connection or of its Secret applies
// to a running leader on restart, and nothing flags it as a failed reload (the stale flag is for reloads that failed): the
// verification of the Bindings tells of tokens that differ from these (ChatCredentialsChanged), and logs a Binding that a reload moved
// to another connection when it stops verifying it.
func ChooseConnection(ctx context.Context, snap *config.Snapshot, own v1alpha1.ObjectRef, secrets identity.SecretReader) (Connection, error) {
	if snap == nil {
		return Connection{}, errors.New("broker: the config has not loaded yet")
	}
	conns := snap.ChatConnections()
	if own != (v1alpha1.ObjectRef{}) {
		i := slices.IndexFunc(conns, func(c *v1alpha1.ChatConnection) bool { return c.Namespace == own.Namespace && c.Name == own.Name })
		if i < 0 {
			return Connection{}, fmt.Errorf("%w: ChatConnection %s is not in the config: AGENTKOURIER_CHAT_CONNECTION names the connection this broker runs", ErrWiring, v1alpha1.NamespacedName(own.Namespace, own.Name))
		}
		conns = conns[i : i+1]
	}
	if len(conns) != 1 {
		names := make([]string, len(conns))
		for i, c := range conns {
			names[i] = v1alpha1.NamespacedName(c.Namespace, c.Name)
		}
		return Connection{}, fmt.Errorf("%w: v1 serves exactly one ChatConnection, and the config has %d [%s]", ErrWiring, len(conns), strings.Join(names, ", "))
	}
	c := conns[0]
	name := v1alpha1.NamespacedName(c.Namespace, c.Name)
	spec, ok := platformFor(c.Spec.Platform)
	if !ok {
		return Connection{}, fmt.Errorf("%w: ChatConnection %s is %q, and v1 serves Slack in Socket Mode", ErrWiring, name, c.Spec.Platform)
	}
	read := func(key string) (secret.Value, error) {
		v, err := secrets.Value(ctx, c.Namespace, c.Spec.CredentialsSecretRef.Name, key)
		if err == nil && v.IsEmpty() {
			err = secret.ErrEmptyValue
		}
		if err != nil {
			// A Secret, key or value that is missing is the operator's to fix, so a usage error. One that
			// cannot be read just now (a cache that has not synced) is not.
			err = fmt.Errorf("ChatConnection %s: read %s of Secret %s: %w", name, key, v1alpha1.NamespacedName(c.Namespace, c.Spec.CredentialsSecretRef.Name), err)
			if errors.Is(err, secret.ErrKeyNotFound) || errors.Is(err, secret.ErrEmptyValue) || errors.Is(err, config.ErrSecretNotFound) {
				err = fmt.Errorf("%w: %w", ErrWiring, err)
			}
			return secret.Value{}, err
		}
		return v, nil
	}
	creds := map[string]secret.Value{}
	for _, key := range spec.Keys {
		v, err := read(key)
		if err != nil {
			return Connection{}, err
		}
		creds[key] = v
	}
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: c.Namespace, Name: c.Name}, Platform: c.Spec.Platform, Credentials: creds}, nil
}
