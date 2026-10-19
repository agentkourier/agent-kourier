package live

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// busyTimeout bounds one call that marks or clears a thread. The mark is a courtesy and the run makes the call between its
// writes, so a platform that is slow to answer must not hold up the reply behind it.
const busyTimeout = 5 * time.Second

// busyMark is a run's marking of its thread as busy (chat.BusyMarker), for a turn on the update path in a progress mode that
// shows activity: set when the run starts, set again after each write the run makes anyway (a post in the thread may take it
// off), and cleared when the run ends however it ends. It adds no write of its own and keeps no timer. A nil *busyMark marks
// nothing, so a run that is not meant to mark calls it all the same. It is the run's goroutine's alone.
type busyMark struct {
	m      chat.BusyMarker
	off    *atomic.Bool // the Renderer's: set once a platform says it cannot, and no turn asks again
	at     chat.Target
	run    *Run
	marked bool // a mark may be on the thread: one was tried and no clear has followed
	done   bool // this turn marks no more
}

// set marks the thread, unless the turn marks no more, the platform has said it cannot, or the turn's context has ended
// (a turn that never ended owes nothing). A platform that says ErrNoBusy has reported why itself, so that is not logged; any
// other error is, once, and this turn stops marking, though it still clears what the failed call may have set.
func (b *busyMark) set(ctx context.Context) {
	if b == nil || b.done || b.off.Load() || ctx.Err() != nil {
		return
	}
	b.marked = true
	if err := b.call(ctx, true); err != nil {
		b.done = true
		if errors.Is(err, chat.ErrNoBusy) {
			b.marked = false // the platform has nothing marked that it can take off
			return
		}
		b.run.cfg.Logger.WarnContext(ctx, "live: marking the thread as busy failed; the turn goes on without it", "channel", b.run.turn.Channel, "err", err)
	}
}

// clear takes the mark off if one may be on. It runs when the run ends, on a context that outlives the turn's: it is what
// leaves the thread unmarked after a turn that was cancelled or failed.
func (b *busyMark) clear(ctx context.Context) {
	if b == nil || !b.marked || b.off.Load() {
		return
	}
	b.marked = false
	if err := b.call(ctx, false); err != nil && !errors.Is(err, chat.ErrNoBusy) {
		b.run.cfg.Logger.WarnContext(ctx, "live: taking the busy mark off the thread failed; it stays until the next message in the thread", "channel", b.run.turn.Channel, "err", err)
	}
}

// call is one MarkBusy, bounded by busyTimeout and not by the turn's context, which a cancelled turn must still be able to clear
// with. An ErrNoBusy turns the mark off for the Renderer.
func (b *busyMark) call(ctx context.Context, busy bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), busyTimeout)
	defer cancel()
	err := b.m.MarkBusy(ctx, b.at, busy)
	if errors.Is(err, chat.ErrNoBusy) {
		b.off.Store(true)
	}
	return err
}
