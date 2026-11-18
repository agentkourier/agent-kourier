package interact

import (
	"context"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// waitCount is how many waits o holds.
func (o *Outcomes) waitCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, w := range o.waits {
		n += len(w)
	}
	return n
}

// A wait hands the press on and returns what its processing publishes: a decision with its words when it is Done, and no decision,
// with no words, when it is to be tried again. Either way the wait is over, and nothing of it is kept. The wait is registered before
// the hand-off, so that a worker that decides the press before the hand-off returns is heard too.
func TestAWaitReturnsWhatTheProcessingPublishes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		res  Result
		text string
		want chat.PressOutcome
	}{
		"decided":        {Done, textNotMember, chat.PressOutcome{Decided: true, Text: textNotMember}},
		"decided, quiet": {Done, "", chat.PressOutcome{Decided: true}},
		"to be retried":  {Retry, textNotMember, chat.PressOutcome{}},
	} {
		for when, publish := range map[string]func(o *Outcomes, seq int64){
			"after the hand-off": func(o *Outcomes, seq int64) { go o.Publish(seq, tc.res, tc.text) },
			"before it returns":  func(o *Outcomes, seq int64) { o.Publish(seq, tc.res, tc.text) },
		} {
			o := NewOutcomes(4)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			var notified []store.Press
			got := o.Wait(ctx, store.Press{Seq: 7}, func(p store.Press) {
				notified = append(notified, p)
				publish(o, p.Seq)
			})
			cancel()
			if got != tc.want || len(notified) != 1 || notified[0].Seq != 7 {
				t.Errorf("%s, published %s: %+v, notified %v; want %+v, notified once", name, when, got, notified, tc.want)
			}
			if n := o.waitCount(); n != 0 {
				t.Errorf("%s, published %s: %d waits kept after the outcome", name, when, n)
			}
		}
	}
}

// A wait whose context ends first returns no decision, and leaves nothing behind: a publish after it finds no one and does not block.
func TestAWaitThatRunsOutReturnsNoDecision(t *testing.T) {
	t.Parallel()
	o := NewOutcomes(4)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	notified := 0
	if got := o.Wait(ctx, store.Press{Seq: 3}, func(store.Press) { notified++ }); got != (chat.PressOutcome{}) || notified != 1 {
		t.Fatalf("%+v, notified %d times; want no decision, notified once", got, notified)
	}
	if n := o.waitCount(); n != 0 {
		t.Errorf("%d waits kept after the context ended", n)
	}
	o.Publish(3, Done, textGone) // nobody waits: it must not block
}

// A press is only published to the wait for its own Seq.
func TestAPublishReachesOnlyItsOwnPress(t *testing.T) {
	t.Parallel()
	o := NewOutcomes(4)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	got := o.Wait(ctx, store.Press{Seq: 1}, func(store.Press) { o.Publish(2, Done, textGone) })
	if got != (chat.PressOutcome{}) {
		t.Errorf("the wait for press 1 got the outcome of press 2: %+v", got)
	}
}

// Past max waits at once a press is handed on at once and not waited for, so that the waits never hold more than their share of the
// requests of the chat listener; a nil Outcomes waits for nothing.
func TestWaitsPastTheCapAndANilOutcomesDoNotWait(t *testing.T) {
	t.Parallel()
	o := NewOutcomes(1)
	ctx, cancel := context.WithCancel(t.Context())
	held := make(chan struct{})
	done := make(chan chat.PressOutcome)
	go func() { done <- o.Wait(ctx, store.Press{Seq: 1}, func(store.Press) { close(held) }) }()
	<-held
	notified := 0
	bounded, stop := context.WithTimeout(t.Context(), time.Second) // a wait past the cap that waits fails here rather than hangs
	defer stop()
	start := time.Now()
	if got := o.Wait(bounded, store.Press{Seq: 2}, func(store.Press) { notified++ }); got != (chat.PressOutcome{}) || notified != 1 {
		t.Errorf("past the cap: %+v, notified %d times; want no decision, notified once", got, notified)
	}
	if time.Since(start) >= time.Second {
		t.Error("a wait past the cap waited")
	}
	cancel()
	<-done

	var none *Outcomes
	notified = 0
	if got := none.Wait(t.Context(), store.Press{Seq: 3}, func(store.Press) { notified++ }); got != (chat.PressOutcome{}) || notified != 1 {
		t.Errorf("a nil Outcomes: %+v, notified %d times; want no decision, notified once", got, notified)
	}
	none.Publish(3, Done, "") // must not panic
}

// A wait gives its place back when it ends, whether its context ran out or the outcome was published: at a cap of one, the next press
// is still waited for and gets its own outcome. Otherwise the cap would fill for good, and every press would go unwaited for.
func TestAWaitGivesItsPlaceBack(t *testing.T) {
	t.Parallel()
	o := NewOutcomes(1)
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if got := o.Wait(short, store.Press{Seq: 1}, func(store.Press) {}); got != (chat.PressOutcome{}) {
		t.Fatalf("the wait that ran out: %+v", got)
	}
	waitedFor := func(seq int64) chat.PressOutcome {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return o.Wait(ctx, store.Press{Seq: seq}, func(p store.Press) {
			go func() { time.Sleep(10 * time.Millisecond); o.Publish(p.Seq, Done, textGone) }() // the worker, a little later
		})
	}
	want := chat.PressOutcome{Decided: true, Text: textGone}
	if got := waitedFor(2); got != want {
		t.Errorf("the press after a wait that ran out: %+v; want %+v, waited for", got, want)
	}
	if got := waitedFor(3); got != want {
		t.Errorf("the press after a wait that was published to: %+v; want %+v, waited for", got, want)
	}
	if n := o.waitCount(); n != 0 {
		t.Errorf("%d waits kept", n)
	}
}

// Close ends every wait with no decision, at once, and a wait after it is not waited for: the broker closes the waits as it stops, so
// that each press still waiting is answered within the chat listener's drain.
func TestCloseEndsTheWaits(t *testing.T) {
	t.Parallel()
	o := NewOutcomes(4)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	held := make(chan struct{})
	done := make(chan chat.PressOutcome, 1)
	go func() { done <- o.Wait(ctx, store.Press{Seq: 1}, func(store.Press) { close(held) }) }()
	<-held
	start := time.Now()
	o.Close()
	if got := <-done; got != (chat.PressOutcome{}) || time.Since(start) > time.Second {
		t.Errorf("a wait at Close: %+v after %v; want no decision at once", got, time.Since(start))
	}
	notified := 0
	start = time.Now()
	if got := o.Wait(ctx, store.Press{Seq: 2}, func(store.Press) { notified++ }); got != (chat.PressOutcome{}) || notified != 1 || time.Since(start) > time.Second {
		t.Errorf("a wait after Close: %+v, notified %d times, after %v; want no decision at once, notified once", got, notified, time.Since(start))
	}
	o.Publish(1, Done, textGone) // nobody waits: it must not block
	if n := o.waitCount(); n != 0 {
		t.Errorf("%d waits kept after Close", n)
	}
	var none *Outcomes
	none.Close() // must not panic
}

// Limit is the cap the Outcomes was made with.
func TestLimitIsTheCap(t *testing.T) {
	t.Parallel()
	if got := NewOutcomes(3).Limit(); got != 3 {
		t.Errorf("Limit = %d; want 3", got)
	}
}
