package live

import (
	"context"
	"errors"
	"log/slog"
	"sync"

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

// queuedKind says what a notice of kind does to the queued reaction of the message it is about (spec 8.2): put it on when the reply
// is queued behind a task or a turn, or take it off (remove) when the turn is sent or ends. ok is false for a kind that is not about it.
func queuedKind(kind session.NoticeKind) (remove, ok bool) {
	switch kind {
	case session.NoticeQueued:
		return false, true
	case session.NoticeDequeued:
		return true, true
	}
	return false, false
}

// Sink is the session manager's Sink. A turn's events go to a Run, whose handler never blocks, so
// the stream reader is never held up by Slack. A notice is only written to the outbox, which posts
// it later: Notice is called on Slack's acknowledgement path, where a post would be a call to Slack
// that Slack waits on.
type Sink struct {
	runs  *Renderer
	queue Enqueuer
	log   *slog.Logger
	// queued is the name of each queued reaction this Sink put on, by the notice's ID, until the notice that takes it off: the removal
	// names the reaction that went on whether or not the connection's platform still runs. The session pairs each queued notice with a
	// dequeued one in the same process (its marked set is in memory too), which deletes the entry. Guarded by mu.
	mu     sync.Mutex
	queued map[string]string
}

// NewSink returns a Sink that renders turns with runs and queues notices on queue. The reaction that marks a reply that waits is
// the platform's own (chat.Caps.AckReaction) of the notice's connection, which runs' Config.Platforms gives; a nil runs knows no
// platform, and queues no such reaction.
func NewSink(runs *Renderer, queue Enqueuer, log *slog.Logger) *Sink {
	if log == nil {
		log = slog.Default()
	}
	return &Sink{runs: runs, queue: queue, log: log, queued: map[string]string{}}
}

// queuedReaction is the reaction that marks a reply that waits in a thread of conn: the platform's own (chat.Caps.AckReaction).
// known is false when no platform runs conn now, and the name is empty, with known true, when the platform has no reactions.
func (s *Sink) queuedReaction(conn string) (name string, known bool) {
	if s.runs == nil {
		return "", false
	}
	p, err := s.runs.cfg.Platforms.Platform(conn)
	if err != nil {
		return "", false
	}
	return p.Capabilities().AckReaction, true
}

// Turn starts the rendering of a turn and returns its handler. ctx is the turn's own, which ends
// when its send returns.
func (s *Sink) Turn(ctx context.Context, t session.TurnRef) agent.Handler {
	return s.start(ctx, t).Handle
}

// TurnCards is Turn, and the cards of the turn: what its message shows of the tool calls a pause for an approval waits on
// (Run.Cards), which the interactions' tap asks once the handler has taken the pause, and which waits for the run's last write.
func (s *Sink) TurnCards(ctx context.Context, t session.TurnRef) (agent.Handler, func(context.Context) map[string]chat.ToolLine) {
	u := s.start(ctx, t)
	return u.Handle, u.Cards
}

// start starts the rendering of the turn t.
func (s *Sink) start(ctx context.Context, t session.TurnRef) *Run {
	recipient, mode := t.UserID, t.Output
	if recipient == session.SystemUser { // an alert has no one to show a stream to
		recipient = ""
	}
	if mode == "" {
		mode = v1alpha1.OutputLive
	}
	turn := Turn{Source: t.MessageID, Connection: t.Key.Connection, Channel: t.Key.Channel, Thread: t.Key.Thread, Mode: mode, Progress: t.Progress, Recipient: recipient, Recovered: t.Recovered}
	turn.Footer = footerOf(t)
	turn.Binding, turn.Mentions, turn.MaxMentions = t.Binding, t.Mentions, t.MaxMentions
	return s.runs.Start(ctx, turn)
}

// FollowUpHint is the line that ends the final message of a turn in a thread that takes only mentioned replies: one of a Binding whose
// chat.threadReplies is mention (D38), and one that a bot's mention (D49) or a person's message (D51) began, whatever the Binding
// says. There a reply that does not mention Agent Kourier is ignored, and nothing else tells a person how to ask a follow-up. It is plain text, with no
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
// The queued reaction is the platform's own (chat.Caps.AckReaction) of the notice's connection: a platform with no reactions gets
// no row, and a connection that no platform runs now is logged and gets none, since the name of the reaction is not known. Its removal
// names the reaction the queued notice put on, remembered by the notice's ID, so it does not need the platform; with none remembered it
// asks the platform, and with no platform either it is dropped quietly, as nothing this Sink put on is left.
//
// What no retry can mend is logged and is not an error: a kind nobody wrote wording for, and a
// notice that is no valid row (a reaction on no message).
func (s *Sink) QueueNotice(ctx context.Context, key session.Key, n session.Notice) error {
	var err error
	var name string
	remove, ok := queuedKind(n.Kind)
	if ok {
		var known bool
		if remove {
			s.mu.Lock()
			name, known = s.queued[n.ID]
			delete(s.queued, n.ID)
			s.mu.Unlock()
		}
		if !known {
			name, known = s.queuedReaction(key.Connection)
		}
		switch {
		case !known && remove: // nothing this Sink put on is left: the platform stopped before the reaction was asked for, or never ran
			s.log.DebugContext(ctx, "live: a removal of the queued reaction was dropped: no platform runs its connection, and no reaction was put on", "connection", key.Connection, "channel", key.Channel)
			return nil
		case !known:
			s.log.WarnContext(ctx, "live: a notice of the queued reaction was dropped: no platform runs its connection now, so the reaction it names is not known", "kind", n.Kind, "connection", key.Connection, "channel", key.Channel)
			return nil
		case name == "": // the platform has no reactions: there is nothing to put on or take off
			return nil
		case !remove:
			s.mu.Lock()
			s.queued[n.ID] = name
			s.mu.Unlock()
		}
	}
	if n.Kind == session.NoticeAcknowledged || n.Kind == session.NoticeUnacknowledged {
		name, remove, ok = n.Reaction, n.Kind == session.NoticeUnacknowledged, true
	}
	if ok {
		r := outbox.Reaction{Source: n.ID, Connection: key.Connection, Channel: key.Channel, TS: n.MessageRef, Name: name}
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
		post := outbox.Post{Source: n.ID, Role: store.OutboxNotice, Connection: key.Connection, Channel: key.Channel, Thread: key.Thread, Out: chat.Say(render.Notice(text, n.Detail))}
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
	case session.NoticeStopUnverified:
		return "I asked the agent to stop the task, but what it answered was too large to read, so I can't tell whether it stopped. This thread no longer waits for it.", true
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
