package broker

import (
	"context"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// quietSink is the renderer's sink without the turns and notices of a connection whose platform has no replies of Agent Kourier's
// (v1alpha1.ChatPlatform.Replies false: the webhook platform, where the agent posts on the service with its own tools). Such a
// platform posts nothing, so a row queued for it would only be abandoned by the outbox, with a warning for each: nothing is queued
// instead. A turn is not rendered (its Binding's output is none, the only output such a platform serves), and a notice (the queued
// and acknowledgment reactions, the expired and Binding-gone notices, a trigger's notes) is dropped, and QueueNotice says it was
// written, so that no event is delivered again for it.
type quietSink struct {
	renderSink
	// quiet says the connection, as "namespace/name", has no replies. It reads the config (noRepliesIn), not the runtime table's
	// Connection: an intake lives and dies with its runtime, but a turn outlives it, going on with the platform it holds while its
	// connection restarts (connRuntime.life), and the table has no runtime of the connection in between, so it could not say that
	// what the turn queues then is for a platform with no replies.
	quiet func(conn string) bool
}

// Turn renders the turn, unless its connection has no replies: then it renders nothing, and the handler is nil.
func (s quietSink) Turn(ctx context.Context, t session.TurnRef) agent.Handler {
	if s.quiet(t.Key.Connection) {
		return nil
	}
	return s.renderSink.Turn(ctx, t)
}

// Notice queues the notice, unless its connection has no replies.
func (s quietSink) Notice(ctx context.Context, key session.Key, n session.Notice) {
	if s.quiet(key.Connection) {
		return
	}
	s.renderSink.Notice(ctx, key, n)
}

// QueueNotice queues the notice and returns the error of the write, unless its connection has no replies: then it queues nothing
// and returns nil.
func (s quietSink) QueueNotice(ctx context.Context, key session.Key, n session.Notice) error {
	if s.quiet(key.Connection) {
		return nil
	}
	return s.renderSink.QueueNotice(ctx, key, n)
}

// noRepliesIn says whether the ChatConnection conn ("namespace/name") of the config that snapshot gives now is on a platform with no
// replies. A connection the config does not have, or one whose platform the API does not know, has replies: its rows go to the outbox,
// which abandons those that no platform can post.
func noRepliesIn(snapshot func() *config.Snapshot) func(conn string) bool {
	return func(conn string) bool {
		snap := snapshot()
		if snap == nil {
			return false
		}
		for _, c := range snap.ChatConnections() {
			if v1alpha1.NamespacedName(c.Namespace, c.Name) != conn {
				continue
			}
			p, ok := v1alpha1.ChatPlatformOf(c.Spec)
			return ok && !p.Replies
		}
		return false
	}
}
