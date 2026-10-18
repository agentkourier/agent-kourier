package live

import (
	"context"
	"errors"
	"log/slog"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Enqueuer writes an outbox row and sends nothing: *outbox.Outbox.
type Enqueuer interface {
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
	EnqueueReaction(ctx context.Context, r outbox.Reaction) (string, error)
	EnqueueUnreaction(ctx context.Context, r outbox.Reaction) (string, error)
}

// Reactions is what a thread's messages show of the state of their turns, which is the one place that says it:
// the sink asks it what a notice does to a message's reactions, and nothing else names an emoji. A name is the
// platform's bare emoji name.
type Reactions struct {
	// Queued marks a reply that waits (spec 8.2). It goes on when the reply is queued behind a task or a
	// turn, and comes off when the turn is sent or ends.
	Queued string
}

// DefaultReactions returns the reactions Agent Kourier uses unless it is told otherwise.
func DefaultReactions() Reactions { return Reactions{Queued: "hourglass_flowing_sand"} }

// of says what a notice of kind does to the reactions of the message it is about: put an emoji on, or take it
// off. ok is false for a kind that is not about a reaction.
func (r Reactions) of(kind session.NoticeKind) (name string, remove, ok bool) {
	switch kind {
	case session.NoticeQueued:
		return r.Queued, false, true
	case session.NoticeDequeued:
		return r.Queued, true, true
	}
	return "", false, false
}

// Sink is the session manager's Sink. A turn's events go to a Run, whose handler never blocks, so
// the stream reader is never held up by Slack. A notice is only written to the outbox, which posts
// it later: Notice is called on Slack's acknowledgement path, where a post would be a call to Slack
// that Slack waits on.
type Sink struct {
	runs      *Renderer
	queue     Enqueuer
	log       *slog.Logger
	reactions Reactions
}

// NewSink returns a Sink that renders turns with runs and queues notices on queue, showing the turns' states
// with DefaultReactions.
func NewSink(runs *Renderer, queue Enqueuer, log *slog.Logger) *Sink {
	if log == nil {
		log = slog.Default()
	}
	return &Sink{runs: runs, queue: queue, log: log, reactions: DefaultReactions()}
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
	progress := t.Progress
	if progress == "" {
		progress = v1alpha1.ProgressStepsAndActivity // the API's default
	}
	turn := Turn{Source: t.MessageID, Channel: t.Key.Channel, Thread: t.Key.Thread, Mode: mode, Progress: progress, Recipient: recipient, Recovered: t.Recovered}
	turn.Footer = footerOf(t)
	return s.runs.Start(ctx, turn).Handle
}

// FollowUpHint is the line that ends the final message of a turn in a Binding whose chat.threadReplies is mention (D38): there, a
// reply that does not mention Agent Kourier is ignored, and nothing else tells a person how to ask a follow-up. It is plain text, with no
// mention syntax of any platform: the neutral model has no way to name Agent Kourier, and a line that said who to mention could not
// be wrong about it.
const FollowUpHint = "Mention me in this thread to ask a follow-up."

// footerOf is what the final message of the turn t ends with, in the order it is read: the follow-up hint, a context line, and
// then the feedback question and its buttons, which are the last thing in the message. The Run puts it on the messages that
// end a turn with something to show, and on no other.
func footerOf(t session.TurnRef) []chat.Part {
	var parts []chat.Part
	if t.ThreadReplies == v1alpha1.ThreadRepliesMention {
		parts = append(parts, chat.Context{Text: FollowUpHint})
	}
	if t.Feedback != "" { // the buttons say what they rate by the value alone, so that a press after a restart finds it
		parts = append(parts, feedback.Parts(t.Feedback, t.Key.Thread, t.MessageID)...)
	}
	return parts
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
// reaction on its message, not a message, and so is the end of its wait (the dequeued notice, which
// takes the reaction off again): each is a row too, and is made later, off the acknowledgement path. The
// two notices share an ID, and the outbox pairs their rows by it, whichever comes first.
//
// What no retry can mend is logged and is not an error: a kind nobody wrote wording for, and a
// notice that is no valid row (a reaction on no message).
func (s *Sink) QueueNotice(ctx context.Context, key session.Key, n session.Notice) error {
	var err error
	name, remove, ok := s.reactions.of(n.Kind)
	if n.Kind == session.NoticeAcknowledged || n.Kind == session.NoticeUnacknowledged {
		name, remove, ok = n.Reaction, n.Kind == session.NoticeUnacknowledged, true
	}
	if ok {
		r := outbox.Reaction{Source: n.ID, Channel: key.Channel, TS: n.MessageRef, Name: name}
		if remove {
			_, err = s.queue.EnqueueUnreaction(ctx, r)
		} else {
			_, err = s.queue.EnqueueReaction(ctx, r)
		}
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

// noticeText is what a notice of the kind says, which Agent Kourier wrote; a notice's Detail is added after
// it. There is none for a kind that is not a message.
func noticeText(kind session.NoticeKind) (string, bool) {
	switch kind {
	case session.NoticeRetrying:
		return "I can't reach the agent right now, and I'm trying again.", true
	case session.NoticeStillBusy:
		return "The agent is still busy with an earlier task in this thread, so this message is waiting. I'll keep trying.", true
	case session.NoticeNotReady:
		return "The agent isn't ready to take this message yet, so it is waiting. I'll keep trying.", true
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
	case session.NoticeTaskFailed:
		return "The agent's task failed.", true
	case session.NoticeTaskRejected:
		return "The agent rejected this request, so there is no answer.", true
	case session.NoticeTaskCanceled:
		return "The agent's task was canceled before it finished.", true
	case session.NoticeNoOutput:
		return "The agent finished without any output.", true
	case session.NoticeTriggerRepeat: // the Detail is the time, and ends the sentence
		return "Fired again at", true
	case session.NoticeTriggerClosed:
		return "Resolved at", true
	case session.NoticeTriggerRefused: // the Detail says which value failed
		return "Not investigated:", true
	}
	return "", false
}
