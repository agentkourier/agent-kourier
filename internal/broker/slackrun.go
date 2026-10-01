package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

var (
	// ErrSlackAuth is wrapped by the error RunSlack returns when Slack rejected the tokens for good:
	// the process exits, with the code in the error, because no retry would help.
	ErrSlackAuth = errors.New("slack rejected the tokens")
	// ErrWiring is wrapped by an error that means the broker was put together wrongly, or its
	// config asks for what v1 does not do: a usage error, not a failure of a dependency.
	ErrWiring = errors.New("the broker cannot be wired")
)

// SlackRunner is *slack.Client: it holds the Socket Mode connection until it fails or ctx ends.
type SlackRunner interface {
	Run(ctx context.Context, sink slack.Sink) error
}

// RunPolicy is how RunSlack paces its retries. Zero fields take their defaults.
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

// RunSlack runs the Slack connection for as long as ctx lives. slack-go reconnects by itself, so
// when Run returns, the connection failed for good or never opened. An error that says the tokens
// are rejected (slack.FatalAuthCode) ends RunSlack at once with ErrSlackAuth, since no retry cures
// it. Any other error is retried after a pause that doubles from Base to Max, and starts over from
// Base once a run has lasted ResetAfter. It returns an error wrapping ctx's when ctx ends: a
// shutdown, not a failure. The errors it logs come from Run, which keeps tokens out of them.
func RunSlack(ctx context.Context, runner SlackRunner, sink slack.Sink, p RunPolicy) error {
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
			return fmt.Errorf("slack run stopped: %w", ctx.Err())
		}
		if code := slack.FatalAuthCode(err); code != "" {
			return fmt.Errorf("%w: %s", ErrSlackAuth, code)
		}
		if now().Sub(started) >= reset {
			pause = min(base, limit)
		}
		log.WarnContext(ctx, "slack connection ended; connecting again", "error", err, "after", pause)
		select {
		case <-ctx.Done():
			return fmt.Errorf("slack run stopped: %w", ctx.Err())
		case <-after(pause):
		}
		pause = min(pause*2, limit)
	}
}

// SlackConnection is the one ChatConnection v1 serves, with its tokens as read at startup.
type SlackConnection struct {
	Ref v1alpha1.ObjectRef
	// BotToken and AppToken are secret.Values: they print as redacted.
	BotToken, AppToken secret.Value
}

// ChooseConnection returns the only ChatConnection of snap, with the botToken and appToken keys of
// its credentials Secret read through secrets. v1 serves exactly one, because the outbox has no
// connection column (Phase 4), and only a Slack connection in Socket Mode. Anything else is an
// error wrapping ErrWiring. The tokens are those of the startup snapshot: a change of the connection
// or of its Secret applies on restart, which the stale flag shows.
func ChooseConnection(ctx context.Context, snap *config.Snapshot, secrets identity.SecretReader) (SlackConnection, error) {
	if snap == nil {
		return SlackConnection{}, errors.New("broker: the config has not loaded yet")
	}
	conns := snap.ChatConnections()
	if len(conns) != 1 {
		names := make([]string, len(conns))
		for i, c := range conns {
			names[i] = v1alpha1.NamespacedName(c.Namespace, c.Name)
		}
		return SlackConnection{}, fmt.Errorf("%w: v1 serves exactly one ChatConnection, and the config has %d [%s]", ErrWiring, len(conns), strings.Join(names, ", "))
	}
	c := conns[0]
	name := v1alpha1.NamespacedName(c.Namespace, c.Name)
	if c.Spec.Platform != v1alpha1.PlatformSlack || (c.Spec.Mode != "" && c.Spec.Mode != v1alpha1.ModeSocket) {
		return SlackConnection{}, fmt.Errorf("%w: ChatConnection %s is %q in mode %q, and v1 serves Slack in Socket Mode", ErrWiring, name, c.Spec.Platform, c.Spec.Mode)
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
	bot, err := read("botToken")
	if err != nil {
		return SlackConnection{}, err
	}
	app, err := read("appToken")
	if err != nil {
		return SlackConnection{}, err
	}
	return SlackConnection{Ref: v1alpha1.ObjectRef{Namespace: c.Namespace, Name: c.Name}, BotToken: bot, AppToken: app}, nil
}
