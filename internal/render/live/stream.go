package live

import (
	"context"
	"errors"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// writeStream takes the next step of a stream, and records how far it got, so a failed step is
// retried from where it stopped, never repeated (an appended text is not appended again, a begun step
// is not begun again, a stopped stream is not stopped again).
//
// Order matters. The outbox row is made first, with no payload, which marks it a stream that Run
// never delivers. Stream creates the message, which metadata cannot yet find (spec 8.6 step
// 6), so its ts is recorded before anything else is sent: a crash in that window leaves one
// orphan stream and a row with no ts, and the recovered turn posts the whole text as a new message
// (R16, accepted). Appends carry only the new suffix: a TextEvent replaces an artifact's text, so
// what is new is what follows the text already sent.
//
// A tool call is a step, shown where in the text it was made: the text that came before it is sent first, then the
// step, begun, and finished by its result. A step already finished by the time it is first sent is begun in its
// outcome, in one call. A step is moved forward and never back, so no step is taken back. Every step is a call to the
// platform, and a platform's rate limit is a few a minute, so a window finishes the steps that have an outcome to show,
// and begins no more than maxBegins new ones, which the next window goes on with (the text waits for them: it is never
// sent ahead of a step that came before it). The final has no such limit, since it is the last window there is.
//
// A step that stands for time is a step like these. A Working… card is begun when the delay passes (live.go, openIdle), and
// the call that claims it moves it on under the call's title, with the call's detail. Every chunk of a step carries its
// detail, as chat.Step says; a platform that shows only the first non-empty one (Slack appends a card's detail) is the
// adapter's to handle, so the card has the detail once.
//
// A platform that refuses the stream for good, as it would a start it can never take, does not cost the reply: the
// turn goes to the post path through the outbox, under a row of its own, as a recovered turn that finds a row with no
// ts does.
//
// The stop sends no blocks, for a platform shows them under the text that was streamed, and the stream already is the
// reply: its text goes out once. A step still under way at the stop has no outcome to show, and it is ended before the
// stop is sent, with the steps, so that the stop is the one call that carries the last of the text and nothing else. If the
// text is no longer a prefix of the state, nothing can be appended: the stream is stopped with no suffix, and an
// Update then replaces the message with the final one, never while the stream runs.
func (u *Run) writeStream(ctx context.Context, ref *chat.Ref, v view) error {
	id := outbox.ID(u.turn.Source, store.OutboxReply)
	if !u.rowMade {
		created, err := u.cfg.Store.CreateOutbox(ctx, store.OutboxEntry{ID: id, EventID: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, Thread: u.turn.Thread, CreatedAt: u.cfg.Now()})
		if err != nil {
			return err
		}
		if !created { // an earlier attempt at this turn started a stream, or tried to
			return u.takeOver(ctx, ref, id)
		}
		u.rowMade = true
	}
	if u.stream == nil {
		first := v.body
		if len(v.steps) > 0 { // the text before the first step: the step comes where its call was made
			first = v.body[:v.pos[0]]
		}
		s, err := u.cfg.Platform.Stream(ctx, chat.Target{Channel: u.turn.Channel, Thread: u.turn.Thread}, u.turn.Recipient, first)
		if err != nil {
			if u.cfg.Platform.Classify(chat.OpPost, err).Class == chat.Permanent {
				u.cfg.Logger.WarnContext(ctx, "live: the platform refused the stream for good; the reply is posted instead", "channel", u.turn.Channel, "err", err)
				return u.postInstead(ctx, ref)
			}
			return err
		}
		u.stream, u.streamed = s, first
	}
	if !u.recorded {
		if err := u.cfg.Store.RecordMessageRef(ctx, id, u.stream.Ref().ID); err != nil {
			return err
		}
		u.recorded = true
	}
	for _, st := range v.steps { // the steps with an outcome or a title to show, which ask nothing of the text
		if sent, begun := u.sentOf(st.ID); begun && (sent.Title != st.Title || (sent.State != st.State && st.State != chat.StepRunning)) {
			if ok, err := u.sendStep(ctx, st, v.final); err != nil {
				return err
			} else if !ok {
				break
			}
		}
	}
	limit, begins := len(v.body), 0 // the text is sent as far as the steps that are begun allow
	for i, st := range v.steps {
		if _, begun := u.sentOf(st.ID); begun {
			continue
		}
		if !v.final && begins == maxBegins {
			limit, u.more = min(limit, v.pos[i]), true
			break
		}
		if err := u.appendText(ctx, v.body, v.pos[i]); err != nil { // what was written before the call
			return err
		}
		ok, err := u.sendStep(ctx, st, v.final)
		if err != nil {
			return err
		}
		if !ok {
			break // the steps after it wait their turn, and the text goes on
		}
		begins++
	}
	if !v.final {
		if err := u.appendText(ctx, v.body, limit); err != nil {
			return err
		}
		u.sent = u.want(u.shown(v))
		return nil
	}
	if !u.stopped {
		suffix, prefix := strings.CutPrefix(v.body, u.streamed)
		if !prefix {
			u.diverged, suffix = true, ""
		}
		if err := u.stream.Stop(ctx, suffix, u.stopFooter(v), id); err != nil {
			return err
		}
		u.stopped = true
	}
	if u.diverged {
		if err := u.cfg.Platform.Update(ctx, u.stream.Ref(), chat.Say(v.m)); err != nil {
			return err
		}
		u.diverged = false
	}
	u.sent = u.want(v)
	return nil
}

// stopFooter is the message a stream is stopped with: the turn's footer, if the stream holds the text it rates. Slack shows the
// stop's blocks under the streamed text. A stream that diverged is stopped bare, as it always was, and the update that replaces
// the message carries the footer (the final view's own message): sent in both, it would be there twice.
func (u *Run) stopFooter(v view) chat.Message {
	if u.diverged || !v.rated || v.body == "" || len(u.turn.Footer) == 0 {
		return chat.Message{}
	}
	return chat.Message{Parts: u.turn.Footer}
}

// appendText sends the text of body up to n, if what was streamed is a prefix of it: a text that is not cannot be
// appended, and waits for the final text.
func (u *Run) appendText(ctx context.Context, body string, n int) error {
	n = min(n, len(body))
	if !strings.HasPrefix(body, u.streamed) || n <= len(u.streamed) {
		return nil
	}
	if err := u.stream.Append(ctx, body[len(u.streamed):n]); err != nil {
		return err
	}
	u.streamed = body[:n]
	return nil
}

// sentOf is the step id as it was last sent, and whether it was sent at all.
func (u *Run) sentOf(id string) (chat.Step, bool) {
	for _, s := range u.stepsSent {
		if s.ID == id {
			return s, true
		}
	}
	return chat.Step{}, false
}

// sentStep records that st was sent.
func (u *Run) sentStep(st chat.Step) {
	for i := range u.stepsSent {
		if u.stepsSent[i].ID == st.ID {
			u.stepsSent[i] = st
			return
		}
	}
	u.stepsSent = append(u.stepsSent, st)
}

// shown is v as the stream shows it now: the text and the steps that have been sent, each as it was sent.
func (u *Run) shown(v view) view {
	out := view{body: u.streamed}
	for _, st := range v.steps {
		if sent, begun := u.sentOf(st.ID); begun {
			out.steps = append(out.steps, sent)
		}
	}
	return out
}

// sendStep sends one step, and says whether the ones after it may be sent. A step is decoration and never costs the
// reply its text, so a step that fails is not an error of the write: the text goes on, and the step is tried again with
// the next change. A platform that refuses one for good, or says it shows none, or fails Attempts in a row, ends the
// steps of the turn. At the final the stop must not wait on a step, so one that fails then is let go, and the steps
// after it are still sent. Before the final a rate limit is none of these: it is an error of the write, which the run
// waits out as the platform asked, and it is no strike against the steps, since the text would be limited as well. At
// the final it is a failure like any other and the step is let go: with many steps left, at the few calls a minute a
// platform allows, the waits would outlast the FinalDeadline and cost the stop, and the last of the text it carries.
// A stream that has ended says so to the text that follows, which is where it is dealt with.
//
// When the steps of a turn are ended, the cards already shown stay in the state they were last sent in, a running one
// among them. UNCONFIRMED(WP-04): how Slack shows such a card once the stream stops.
func (u *Run) sendStep(ctx context.Context, st chat.Step, final bool) (bool, error) {
	err := u.stream.Step(ctx, st)
	if err == nil {
		u.sentStep(st)
		u.stepFails = 0
		return true, nil
	}
	verdict := u.cfg.Platform.Classify(chat.OpPost, err)
	switch {
	case errors.Is(err, chat.ErrNotStreaming):
		return false, nil
	case errors.Is(err, chat.ErrNoSteps): // the platform has no steps for this stream, which is no failure
		u.cfg.Logger.InfoContext(ctx, "live: the stream shows no steps; the turn goes on without them", "channel", u.turn.Channel)
		u.stepsOff, u.stepsSent = true, nil
		return false, nil
	case verdict.RetryAfter > 0 && !final:
		return false, err
	case verdict.Class != chat.Transient:
		u.cfg.Logger.WarnContext(ctx, "live: the platform refused a step; the turn goes on without steps", "channel", u.turn.Channel, "err", err)
		u.stepsOff, u.stepsSent = true, nil
		return false, nil
	}
	u.stepFails++
	u.cfg.Logger.WarnContext(ctx, "live: a step could not be shown", "channel", u.turn.Channel, "err", err, "failures", u.stepFails)
	switch {
	case final: // the stop does not wait for it
		u.sentStep(st)
		return true, nil
	case u.stepFails >= u.cfg.Attempts:
		u.cfg.Logger.WarnContext(ctx, "live: giving up on steps; the turn goes on without them", "channel", u.turn.Channel)
		u.stepsOff, u.stepsSent = true, nil
	}
	return false, nil
}

// takeOver carries on a turn whose stream row exists already. With the ts recorded, the stream of
// the earlier attempt is the message: it is stopped, if it still runs, and updated to the whole
// text. It shows no steps of its own, so none of the earlier attempt's is shown twice: a step is begun only on
// the stream this run started. With none, the earlier attempt may have started a stream that cannot be found (R16): the
// whole text is posted as a new message, under a row of its own.
// UNCONFIRMED(WP-04): whether the update that follows the stop of an adopted stream removes the cards the earlier run
// showed, or leaves them as they were last sent.
func (u *Run) takeOver(ctx context.Context, ref *chat.Ref, id string) error {
	row, err := u.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return err
	}
	if row.MessageRef != "" {
		u.streaming = false
		*ref, u.adoptStop = u.cfg.Platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef), true
		return u.writeMoved(ctx, ref)
	}
	return u.postInstead(ctx, ref)
}

// postInstead gives up the stream and posts the reply through the outbox, under a row of its own: the row of the
// stream has no payload, and is not one that Run ever delivers.
func (u *Run) postInstead(ctx context.Context, ref *chat.Ref) error {
	u.streaming = false
	u.turn.Source = outbox.RepostedSource(u.turn.Source)
	return u.writeMoved(ctx, ref)
}

// writeMoved writes a turn that has just moved from the stream to the update path. The view it was given was the stream's,
// which shows a tool call as a step and a wait as a card, and so may have no message at all; the update path's own view
// shows them as status lines, and is the one to write. A state with nothing to show yet writes nothing, and a turn that
// ended with nothing says so, as the run does.
func (u *Run) writeMoved(ctx context.Context, ref *chat.Ref) error {
	v := u.snapshot()
	if !v.ok {
		if !v.final {
			return nil
		}
		v.m, v.body, v.ok = render.Nothing(), render.NoOutput, true
	}
	return u.write(ctx, ref, v)
}
