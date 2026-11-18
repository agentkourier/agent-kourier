package interact

import (
	"context"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// waits is how many waits o holds.
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
