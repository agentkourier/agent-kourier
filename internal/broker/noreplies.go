package broker

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
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

// The renderer's sink says what a paused turn's reply shows of the calls the pause waits on (interact.CardSink), and the quiet sink
// passes it on to the tap, which takes a sink without it and goes without: these hold both to it.
var (
	_ interact.CardSink = (*live.Sink)(nil)
	_ interact.CardSink = quietSink{}
)

// TurnCards is Turn, and the cards of the turn (interact.CardSink), which the renderer's sink says (live.Sink): the tap hands
// them on with a pause, so that the ending of an approval leaves out what a card shows. A connection with no replies has no turn
// rendered and no cards, and a sink that says nothing of cards gives none.
func (s quietSink) TurnCards(ctx context.Context, t session.TurnRef) (agent.Handler, func() map[string]string) {
	if s.quiet(t.Key.Connection) {
		return nil, nil
	}
	if cs, ok := s.renderSink.(interact.CardSink); ok {
		return cs.TurnCards(ctx, t)
	}
	return s.renderSink.Turn(ctx, t), nil
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

// quietOutbox is the outbox of the interactions without the posts of a connection whose platform has no replies (quietSink's twin):
// the interactions write their notices (a refused pause, an answer that came too late or never arrived) to the outbox and not through
// the sink, so they are held here. Enqueue writes nothing for such a connection and returns no ID and no error, so that what asked
// for the notice goes on as if it were written, and Send refuses the post with an error wrapping chat.ErrInvalid, as such a
// platform's Fits refuses the prompt before it is sent.
type quietOutbox struct {
	interact.Sender
	quiet func(conn string) bool // noRepliesIn, as for quietSink
}

// Enqueue writes the post for the outbox to deliver, unless its connection has no replies: then it writes nothing and returns "", nil.
func (o quietOutbox) Enqueue(ctx context.Context, p outbox.Post) (string, error) {
	if o.quiet(p.Connection) {
		return "", nil
	}
	return o.Sender.Enqueue(ctx, p)
}

// Send posts at once, unless the post's connection has no replies: then it posts nothing and returns an error wrapping
// chat.ErrInvalid.
func (o quietOutbox) Send(ctx context.Context, p outbox.Post) (chat.Ref, error) {
	if o.quiet(p.Connection) {
		return chat.Ref{}, fmt.Errorf("broker: connection %s has no replies, so nothing is posted on it: %w", p.Connection, chat.ErrInvalid)
	}
	return o.Sender.Send(ctx, p)
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
