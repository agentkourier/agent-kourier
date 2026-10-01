package live

import (
	"context"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// writeStream takes the next step of a stream, and records how far it got, so a failed step is
// retried from where it stopped, never repeated (an appended text is not appended again, a stopped
// stream is not stopped again).
//
// Order matters. The outbox row is made first, with no payload, which marks it a stream that Run
// never delivers. startStream creates the message, which metadata cannot yet find (spec 8.6 step
// 6), so its ts is recorded before anything else is sent: a crash in that window leaves one
// orphan stream and a row with no ts, and the recovered turn posts the whole text as a new message
// (R16, accepted). Appends carry only the new suffix: a TextEvent replaces an artifact's text, so
// what is new is what follows the text already sent. If the text is no longer a prefix of the
// state, nothing can be appended: the stream is stopped with the final blocks, and a chat.update
// then corrects the text, never while the stream runs.
func (u *Run) writeStream(ctx context.Context, ts *string, c slack.Content, body string, final bool) error {
	id := outbox.ID(u.turn.Source, store.OutboxReply)
	if !u.rowMade {
		created, err := u.cfg.Store.CreateOutbox(ctx, store.OutboxEntry{ID: id, EventID: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, ThreadTS: u.turn.ThreadTS, CreatedAt: u.cfg.Now()})
		if err != nil {
			return err
		}
		if !created { // an earlier attempt at this turn started a stream, or tried to
			return u.takeOver(ctx, ts, c, body, final, id)
		}
		u.rowMade = true
	}
	if u.streamTS == "" {
		got, err := u.cfg.Streamer.StartStream(ctx, u.turn.Channel, u.turn.ThreadTS, u.turn.Recipient, body)
		if err != nil {
			return err
		}
		u.streamTS, u.streamed = got, body
	}
	if !u.recorded {
		if err := u.cfg.Store.RecordMessageRef(ctx, id, u.streamTS); err != nil {
			return err
		}
		u.recorded = true
	}
	suffix, prefix := strings.CutPrefix(body, u.streamed)
	if !final {
		if !prefix {
			return nil // it cannot be appended: wait for the final text
		}
		if suffix != "" {
			if err := u.cfg.Streamer.AppendStream(ctx, u.turn.Channel, u.streamTS, suffix); err != nil {
				return err
			}
			u.streamed = body
		}
		u.sent = u.want(c, u.streamed, false)
		return nil
	}
	if !u.stopped {
		if !prefix {
			u.diverged, suffix = true, ""
		}
		if err := u.cfg.Streamer.StopStream(ctx, u.turn.Channel, u.streamTS, suffix, c.Blocks, id); err != nil {
			return err
		}
		u.stopped = true
	}
	if u.diverged {
		if err := u.cfg.Updater.Update(ctx, u.turn.Channel, u.streamTS, c); err != nil {
			return err
		}
		u.diverged = false
	}
	u.sent = u.want(c, body, true)
	return nil
}

// takeOver carries on a turn whose stream row exists already. With the ts recorded, the stream of
// the earlier attempt is the message: it is stopped, if it still runs, and updated to the whole
// text. With none, the earlier attempt may have started a stream that cannot be found (R16): the
// whole text is posted as a new message, under a row of its own.
func (u *Run) takeOver(ctx context.Context, ts *string, c slack.Content, body string, final bool, id string) error {
	row, err := u.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return err
	}
	u.streaming = false
	if row.MessageRef != "" {
		*ts, u.adoptStop = row.MessageRef, true
	} else {
		u.turn.Source += "/recovered"
	}
	return u.write(ctx, ts, c, body, final)
}
