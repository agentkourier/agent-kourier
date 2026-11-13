package live

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// want is the key of what the state would show: the message's content, or for a stream its text, its steps and
// whether it has ended.
func (u *Run) want(v view) string {
	if u.streaming {
		return v.body + "\x00" + stepsKey(v.steps) + map[bool]string{false: "", true: "\x00final"}[v.final]
	}
	return keyOf(u.message(v))
}

// keyOf is m as want keys a message.
func keyOf(m chat.Message) string {
	key, _ := json.Marshal(m) // a message of parts of known kinds always marshals
	return string(key)
}

// message is the message an update writes for v: v's own, after the steps of v while the turn shows them. A stream's
// steps are the stream's, and a message that replaces the stream must carry them, for a platform may drop the stream's
// cards when the message is updated, as Slack does (probed 2026-10-21): the update of a stream that diverged, and every
// update of a stream that this turn took over (carries). A listed turn's updates carry its steps too, under a line that
// counts the ones it no longer shows. No post carries them: what the outbox stores is v.posted().
func (u *Run) message(v view) chat.Message {
	if len(v.steps) == 0 || u.stepsOff {
		return v.m
	}
	m := v.m
	m.Parts = []chat.Part{chat.Steps{Steps: v.steps}}
	if v.earlier > 0 {
		m.Parts = append([]chat.Part{chat.Context{Text: render.EarlierSteps(v.earlier)}}, m.Parts...)
	}
	m.Parts = append(m.Parts, v.m.Parts...)
	if m.Fallback == "" { // an update with no fallback would leave the one the message had, such as a status line, as its text
		m.Fallback = cmp.Or(v.post.Fallback, v.steps[len(v.steps)-1].Title)
	}
	return m
}

// update replaces the message at ref with the message of v. The steps never cost the reply its text: a message that
// carries them and that the platform refuses as content it cannot show is sent again at once without them, with a
// warning, and the turn shows no steps from then on. Today a refusal of that kind is never retried, so without this the
// steps would take the answer down with them. What is sent again is the message as a post shows it (posted): the status
// line, which the steps stood in for, is back, and a final whose steps were all it showed says "(no output)". A message
// that shows its steps records them as shown (sentStep), so that the final does not drop a card it showed.
//
// A message may hold text this run does not have, an earlier attempt's or its placeholder, and a platform may refuse to put
// cards or a status line in its place (ErrWouldDropText). No retry mends that, and text comes first: the run makes no more
// updates until the turn has text, which the platform takes, and shows its cards then. Each run finds this out once, by one
// refused call, and says so with errMessageStands. An update with text that is refused has lost the reply, and is a failed
// write like any other.
func (u *Run) update(ctx context.Context, ref chat.Ref, v view) error {
	carried := len(v.steps) > 0 && !u.stepsOff
	err := u.platform.Update(ctx, ref, chat.Say(u.message(v)))
	if err == nil && carried {
		for _, st := range v.steps {
			u.sentStep(st)
		}
	}
	if err != nil && carried && u.cannotShow(err) {
		u.cfg.Logger.WarnContext(ctx, "live: the platform refused the message with its steps; it is sent again without them", "channel", u.turn.Channel, "err", err)
		u.stepsOff = true
		err = u.platform.Update(ctx, ref, chat.Say(v.posted()))
	}
	switch {
	case errors.Is(err, chat.ErrWouldDropText) && !hasBody(v.m):
		u.cfg.Logger.InfoContext(ctx, "live: the message holds text that an update with none would take away; it stands until the turn has text", "channel", u.turn.Channel)
		u.holdsText = true
		return errMessageStands
	case err == nil && hasBody(v.m):
		u.holdsText = false // the text is now this run's own, and the next update is the platform's to judge again
	}
	return err
}

// hasBody says whether m holds text. It reads the message and not the view's body, which for a stream's stop can be the placeholder
// (run): what the platform judges is the message.
func hasBody(m chat.Message) bool {
	return slices.ContainsFunc(m.Parts, func(p chat.Part) bool { _, ok := p.(chat.Body); return ok })
}

// cannotShow says whether err is the platform's refusal of a message's content: one it found malformed, too large, or of a
// form it does not take.
func (u *Run) cannotShow(err error) bool {
	if errors.Is(err, chat.ErrInvalid) || errors.Is(err, chat.ErrTooLarge) {
		return true
	}
	v := u.platform.Classify(chat.OpPost, err)
	return v.Class == chat.Permanent && v.Reason == chat.ReasonInvalidContent
}

// stepsKey is the steps by ID, state and title: what a stream shows of them.
func stepsKey(steps []chat.Step) string {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "%s:%d:%s;", s.ID, s.State, s.Title)
	}
	return b.String()
}

// write posts the message the first time, through the outbox, and updates it after, or, on the
// stream path, takes the next step of the stream.
func (u *Run) write(ctx context.Context, ref *chat.Ref, v view) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.render.write", attribute.Bool("agentkourier.render.final", v.final), attribute.Bool("agentkourier.render.streaming", u.streaming))
	defer func() {
		spanErr := telemetryErr
		if errors.Is(spanErr, errNothingToShow) || errors.Is(spanErr, errMessageStands) { // a normal end or wait, not a failed write
			spanErr = nil
		}
		op.Finish(spanErr)
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.WriteTimeout) // see WriteTimeout
	defer cancel()
	if u.streaming {
		return u.writeStream(ctx, ref, v)
	}
	if ref.ID != "" {
		if u.adoptStop { // an earlier run's stream: end it, or the platform refuses the update
			err := u.platform.StreamAt(*ref).Stop(ctx, "", chat.Message{}, outbox.ID(u.turn.Source, store.OutboxReply))
			if err != nil && !errors.Is(err, chat.ErrNotStreaming) {
				return err
			}
			u.adoptStop = false
		}
		err := u.update(ctx, *ref, v)
		switch {
		case err != nil:
		case u.stepsOff && len(v.steps) > 0: // the steps were refused, and the message was sent again as a post shows it
			u.sent = keyOf(v.posted())
		default:
			u.sent = u.want(v)
		}
		return err
	}
	u.sends++
	earlier := u.sends == 1 && u.rowMadeBefore(ctx)
	posted := v.posted()
	got, err := u.cfg.Sender.Send(ctx, outbox.Post{Source: u.turn.Source, Role: store.OutboxReply, Connection: u.turn.Connection, Channel: u.turn.Channel, Thread: u.turn.Thread, Out: chat.Say(posted)})
	if err == nil {
		*ref = got
		// A Send that follows a failed one, or that finds the turn's row made already (by an earlier attempt, here or in an
		// earlier process), returns the message that is there, or posts the payload stored first, not necessarily this content.
		u.stale = u.sends > 1 || earlier
		// The post of a listed turn has its status line where its updates have the steps: the next window, which the throttle
		// spaces, writes them, the final's included.
		u.sent = keyOf(posted)
		u.more = u.more || u.sent != u.want(v)
	}
	return err
}

// rowMadeBefore says whether the outbox row this turn posts under is there before its first post: an earlier attempt at the turn
// made it, and may have posted a message that the post then returns. A read that fails says so too: at worst one more write.
func (u *Run) rowMadeBefore(ctx context.Context) bool {
	_, err := u.cfg.Store.GetOutbox(ctx, outbox.ID(u.turn.Source, store.OutboxReply))
	return !errors.Is(err, store.ErrNotFound)
}
