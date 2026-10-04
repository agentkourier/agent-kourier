package live

import (
	"context"
	"errors"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
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
// outcome, in one call. A step is moved forward and never back, so no step is taken back.
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
			return u.takeOver(ctx, ref, v, id)
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
	for i, st := range v.steps {
		if sent, begun := u.stepState(st.ID); begun && (sent == st.State || st.State == chat.StepRunning) {
			continue // nothing is new of it
		} else if !begun {
			if err := u.appendText(ctx, v.body, v.pos[i]); err != nil { // what was written before the call
				return err
			}
		}
		if !u.sendStep(ctx, st, v.final) {
			break // the steps after it wait their turn, and the text goes on
		}
	}
	if !v.final {
		if err := u.appendText(ctx, v.body, len(v.body)); err != nil {
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
		if err := u.stream.Stop(ctx, suffix, chat.Message{}, id); err != nil {
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

// stepState is the state the step id was last sent in, and whether it was sent at all.
func (u *Run) stepState(id string) (chat.StepState, bool) {
	for _, s := range u.stepsSent {
		if s.ID == id {
			return s.State, true
		}
	}
	return 0, false
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

// shown is v as the stream shows it now: the text and the steps that have been sent, each in the state it was sent in.
func (u *Run) shown(v view) view {
	out := view{body: u.streamed}
	for _, st := range v.steps {
		if sent, begun := u.stepState(st.ID); begun {
			st.State = sent
			out.steps = append(out.steps, st)
		}
	}
	return out
}

// sendStep sends one step, and says whether the ones after it may be sent. A step is decoration and never costs the
// reply its text, so a step that fails is not an error of the write: the text goes on, and the step is tried again with
// the next change. A platform that refuses one for good, or says it shows none, or fails Attempts in a row, ends the
// steps of the turn. At the final the stop must not wait on a step, so one that fails then is let go. A stream that has
// ended says so to the text that follows, which is where it is dealt with.
func (u *Run) sendStep(ctx context.Context, st chat.Step, final bool) bool {
	err := u.stream.Step(ctx, st)
	switch {
	case err == nil:
		u.sentStep(st)
		u.stepFails = 0
		return true
	case errors.Is(err, chat.ErrNotStreaming):
		return false
	case errors.Is(err, chat.ErrNoSteps), u.cfg.Platform.Classify(chat.OpPost, err).Class != chat.Transient:
		u.cfg.Logger.WarnContext(ctx, "live: the platform refused a step; the turn goes on without steps", "channel", u.turn.Channel, "err", err)
		u.stepsOff, u.stepsSent = true, nil
		return false
	}
	u.stepFails++
	u.cfg.Logger.WarnContext(ctx, "live: a step could not be shown", "channel", u.turn.Channel, "err", err, "failures", u.stepFails)
	switch {
	case final: // the stop does not wait for it
		u.sentStep(st)
		return true
	case u.stepFails >= u.cfg.Attempts:
		u.cfg.Logger.WarnContext(ctx, "live: giving up on steps; the turn goes on without them", "channel", u.turn.Channel)
		u.stepsOff, u.stepsSent = true, nil
	}
	return false
}

// takeOver carries on a turn whose stream row exists already. With the ts recorded, the stream of
// the earlier attempt is the message: it is stopped, if it still runs, and updated to the whole
// text. It shows no steps of its own, so none of the earlier attempt's is shown twice: a step is begun only on
// the stream this run started. With none, the earlier attempt may have started a stream that cannot be found (R16): the
// whole text is posted as a new message, under a row of its own.
func (u *Run) takeOver(ctx context.Context, ref *chat.Ref, v view, id string) error {
	row, err := u.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return err
	}
	u.streaming = false
	if row.MessageRef != "" {
		*ref, u.adoptStop = u.cfg.Platform.RefAt(u.turn.Channel, u.turn.Thread, row.MessageRef), true
	} else {
		u.turn.Source += "/recovered"
	}
	return u.write(ctx, ref, v)
}
