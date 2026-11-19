package interact

import (
	"context"
	"sync"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Outcomes hands what the processing of a recorded press came to (chat.PressOutcome) to a caller that waits for it in the platform's
// request (chat.InlineDecider). The press worker processes the press as it processes every press, and the processor publishes what
// it came to (Publish); a waiter only reads it, so a press is decided once, by the worker, whether its waiter is still there or ran
// out of time. It is safe for concurrent use. A nil *Outcomes waits for nothing: Wait hands the press on and returns at once.
type Outcomes struct {
	max int

	mu     sync.Mutex
	waits  map[int64][]chan chat.PressOutcome // by press Seq; guarded by mu
	held   int                                // the waits in waits; guarded by mu
	closed bool                               // Close was called: no wait is registered; guarded by mu
}

// NewOutcomes returns an Outcomes that holds at most limit waits at once. Each wait holds the request of the platform that waits, so
// the cap keeps the waits to their share of the requests the chat listener handles at once.
func NewOutcomes(limit int) *Outcomes {
	return &Outcomes{max: limit, waits: map[int64][]chan chat.PressOutcome{}}
}

// Wait hands press, which is recorded, to notify (the worker's Submit), and waits until ctx ends for what its processing publishes.
// It returns no decision when ctx ends first, when the processing found the press could not be decided yet (Retry), when o is closed
// while it waits, and, without waiting, when max waits are held already, o is closed, or o is nil. The wait is registered before
// notify is called, so a press processed at once is not missed. A press handed over and processed before (a delivery of it again,
// which the worker skips) is not published again, and its wait runs out.
func (o *Outcomes) Wait(ctx context.Context, press store.Press, notify func(store.Press)) chat.PressOutcome {
	ch, ok := o.await(press.Seq)
	if !ok {
		notify(press)
		return chat.PressOutcome{}
	}
	defer o.drop(press.Seq, ch)
	notify(press)
	select {
	case out := <-ch:
		return out
	case <-ctx.Done():
		return chat.PressOutcome{}
	}
}

// Publish hands what the processing of the press seq came to, res and the words for its presser (text, kept only when res is Done),
// to the waits for it, if there are any, and ends them. It never blocks.
func (o *Outcomes) Publish(seq int64, res Result, text string) {
	if o == nil {
		return
	}
	out := chat.PressOutcome{Decided: res == Done}
	if out.Decided {
		out.Text = text
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ch := range o.waits[seq] {
		ch <- out // each has room for one, and is published to once: it leaves the map here
	}
	o.held -= len(o.waits[seq])
	delete(o.waits, seq)
}

// Limit is the most waits o holds at once.
func (o *Outcomes) Limit() int { return o.max }

// Close ends every wait with no decision, at once, and makes every later Wait hand its press on and return at once: the broker closes
// its Outcomes as its chat listener begins to drain at shutdown, so that a press still waiting is answered, as not decided, before the
// drain cuts its request. The presses are still the worker's to decide. Close on a nil Outcomes does nothing.
func (o *Outcomes) Close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	for seq, chs := range o.waits {
		for _, ch := range chs {
			ch <- chat.PressOutcome{} // each has room for one, and leaves the map here
		}
		delete(o.waits, seq)
	}
	o.held = 0
}

// await registers a wait for the press seq, and says false when o is nil, is closed, or holds max waits already.
func (o *Outcomes) await(seq int64) (chan chat.PressOutcome, bool) {
	if o == nil {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.held >= o.max {
		return nil, false
	}
	ch := make(chan chat.PressOutcome, 1)
	o.waits[seq] = append(o.waits[seq], ch)
	o.held++
	return ch, true
}

// drop ends the wait ch for the press seq, if Publish has not.
func (o *Outcomes) drop(seq int64, ch chan chat.PressOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	waits := o.waits[seq]
	for i, w := range waits {
		if w == ch {
			o.waits[seq] = append(waits[:i:i], waits[i+1:]...)
			o.held--
			break
		}
	}
	if len(o.waits[seq]) == 0 {
		delete(o.waits, seq)
	}
}
