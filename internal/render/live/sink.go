package live

import (
	"context"
	"errors"
	"log/slog"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Enqueuer writes an outbox row and sends nothing: *outbox.Outbox.
type Enqueuer interface {
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
	EnqueueReaction(ctx context.Context, r outbox.Reaction) (string, error)
}

// queuedReaction is the emoji that marks a reply waiting for the active task (spec 8.2).
const queuedReaction = "hourglass_flowing_sand"

// Sink is the session manager's Sink. A turn's events go to a Run, whose handler never blocks, so
// the stream reader is never held up by Slack. A notice is only written to the outbox, which posts
// it later: Notice is called on Slack's acknowledgement path, where a post would be a call to Slack
// that Slack waits on.
type Sink struct {
	runs  *Renderer
	queue Enqueuer
	log   *slog.Logger
}

// NewSink returns a Sink that renders turns with runs and queues notices on queue.
func NewSink(runs *Renderer, queue Enqueuer, log *slog.Logger) *Sink {
	if log == nil {
		log = slog.Default()
	}
	return &Sink{runs, queue, log}
}

// Turn starts the rendering of a turn and returns its handler. ctx is the turn's own, which ends
// when its send returns.
func (s *Sink) Turn(ctx context.Context, t session.TurnRef) agent.Handler {
	recipient, mode := t.UserID, t.Output
	if recipient == session.SystemUser { // an alert has no one to show a stream to
		recipient = ""
	}
	if mode == "" {
		mode = v1alpha1.OutputLive
	}
	return s.runs.Start(ctx, Turn{Source: t.MessageID, Channel: t.Key.Channel, Thread: t.Key.Thread, Mode: mode, Recipient: recipient, Recovered: t.Recovered}).Handle
}

// Notice is QueueNotice for the session manager's Sink: it has no way to retry, so a notice that could
// not be written is logged and dropped.
func (s *Sink) Notice(ctx context.Context, key session.Key, n session.Notice) {
	if err := s.QueueNotice(ctx, key, n); err != nil {
		s.log.WarnContext(ctx, "live: a notice was not queued", "kind", n.Kind, "channel", key.Channel, "err", err)
	}
}

// QueueNotice queues the notice in the thread, and returns the error of the write if it failed, for a
// caller that can try again, such as the Slack intake, which then leaves its event unacknowledged. Its
// row is named after the notice's own ID, so the same notice, raised or retried any number of times,
// is one row and one post. A reply that waits for the active task (the queued notice) is shown by a
// reaction on its message, not a message: that is a row too, and is made later, off the
// acknowledgement path.
//
// What no retry can mend is logged and is not an error: a kind nobody wrote wording for, and a
// notice that is no valid row (a reaction on no message).
func (s *Sink) QueueNotice(ctx context.Context, key session.Key, n session.Notice) error {
	var err error
	if n.Kind == session.NoticeQueued {
		_, err = s.queue.EnqueueReaction(ctx, outbox.Reaction{Source: n.ID, Channel: key.Channel, TS: n.MessageRef, Name: queuedReaction})
	} else {
		text, ok := noticeText(n.Kind)
		if !ok { // a kind nobody wrote wording for
			s.log.WarnContext(ctx, "live: a notice of a kind with no wording was dropped", "kind", n.Kind, "channel", key.Channel)
			return nil
		}
		post := outbox.Post{Source: n.ID, Role: store.OutboxNotice, Channel: key.Channel, Thread: key.Thread, Out: chat.Say(render.Notice(text, n.Detail))}
		_, err = s.queue.Enqueue(ctx, post)
	}
	if errors.Is(err, store.ErrInvalid) {
		s.log.WarnContext(ctx, "live: a notice that is no valid row was dropped", "kind", n.Kind, "channel", key.Channel, "err", err)
		return nil
	}
	return err
}

// noticeText is what a notice of the kind says, which courier wrote; a notice's Detail is added after
// it. There is none for a kind that is not a message.
func noticeText(kind session.NoticeKind) (string, bool) {
	switch kind {
	case session.NoticeRetrying:
		return "I can't reach the agent right now, and I'm trying again.", true
	case session.NoticeFailed:
		return "This request failed, and I won't try it again.", true
	case session.NoticeSessionGone:
		return "The agent no longer has this conversation, so it has lost its memory of it. Your next message starts a fresh one.", true
	case session.NoticeBindingGone:
		return "This thread's agent is no longer set up, so I can't pass your messages on.", true
	case session.NoticeExpired:
		return "This conversation has expired, so I can't continue it. Mention me in the channel to start a new one.", true
	case session.NoticeAnswerRejected:
		return "The agent rejected that answer, so I cancelled its task. You can ask again.", true
	case session.NoticeAnswerDropped:
		return "That answer arrived after the task had ended, so I did not send it.", true
	case session.NoticeStopped:
		return "The task was stopped.", true
	case session.NoticeWedged:
		return "The task ran for too long without progress, so I cancelled it.", true
	case session.NoticeDuplicateRun:
		return "The agent ran this request more than once. Check that its work has not been done twice.", true
	}
	return "", false
}
