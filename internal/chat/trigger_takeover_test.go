package chat_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// queuedTurns is the durable record of which turns were taken, as the reply queue is: the start hook adds a turn when
// it takes it, and the Intake asks (IntakeConfig.TurnQueued).
type queuedTurns struct {
	mu  sync.Mutex
	ids map[string]bool
	err error
	ask []string
}

func (q *queuedTurns) add(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = map[string]bool{}
	}
	q.ids[id] = true
}

func (q *queuedTurns) has(_ context.Context, id string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ask = append(q.ask, id)
	return q.ids[id], q.err
}

func (q *queuedTurns) asked() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.ask)
}

func turnID(ts string) string {
	return chat.MessageID(chat.Ref{Platform: "slack", Conn: conn, Channel: "C1", ID: ts})
}

// takeover is a triggered whose start hook takes the turn into q, and whose first mark of a started thread fails, so
// that the turn is queued and the thread is not marked started: the window between the two writes, in which the event
// is acknowledged by nobody and, here, never delivered again.
func newTakeover(t *testing.T) (*triggered, *queuedTurns) { return newTakeoverMarking(t, true) }

// newTakeoverMarking is newTakeover whose first mark fails only if failMark.
func newTakeoverMarking(t *testing.T, failMark bool) (*triggered, *queuedTurns) {
	t.Helper()
	h := newTriggered(t, trialTrigger())
	q := &queuedTurns{}
	cfg := h.cfg
	if failMark {
		cfg.TriggerThreads = &markFailsOnce{TriggerThreads: h.st}
	}
	cfg.TurnQueued = q.has
	cfg.OnStart = func(ctx context.Context, m chat.TriggerMatch) error {
		if err := h.hook(ctx, m); err != nil {
			return err
		}
		q.add(turnID(m.Message.At.ID))
		return nil
	}
	h.cfg = cfg
	h.in = chat.NewIntake(cfg)
	return h, q
}

// A turn that was queued, and whose mark was lost, is the key's investigation: a later fire that finds the reservation
// stale does not take the key over and start a second turn. It marks the first thread started, as the lost write
// would have, and is a repeat in it; the cooldown runs from the first message, and the resolve closes that thread.
func TestAFireDoesNotTakeOverAThreadWhoseTurnWasQueued(t *testing.T) {
	h, q := newTakeover(t)
	if err := h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring)); err == nil {
		t.Fatal("Message = nil; want the mark's error")
	}
	if !q.ids[turnID("100.1")] {
		t.Fatal("the first turn was not queued: the test does not set up the window")
	}

	h.clk.advance(2 * time.Minute) // past the grace: the reservation is stale, and nobody redelivered the first event
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Fatalf("a fire past the grace: %q; want repeat in the first thread, not a second investigation", got)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1"}) {
		t.Errorf("turns started for %v; want only the first", got)
	}
	if kinds, threads := h.noted(); !slices.Equal(threads, []string{"100.1"}) || !slices.Equal(kinds, []session.NoticeKind{session.NoticeTriggerRepeat}) {
		t.Errorf("notes %v in %v; want a repeat in the first thread", kinds, threads)
	}
	if got := h.send(t, "102.1", kindResolved); got != "closed" {
		t.Errorf("the resolve: %q; want closed: the first thread's turn started, so there is an investigation to note", got)
	}
	// The cooldown is the first turn's, four hours from its message at noon, and no later.
	h.clk.advance(3*time.Hour + 50*time.Minute) // 15:52
	if got := h.send(t, "103.1", kindFiring); got != "repeat" {
		t.Errorf("a fire inside the first turn's cooldown: %q; want repeat", got)
	}
	h.send(t, "104.1", kindResolved)
	h.clk.advance(9 * time.Minute) // 16:01: past the first turn's cooldown (16:00), inside the cooldown a lazy adoption from the second fire would give (16:02)
	if got := h.send(t, "105.1", kindFiring); got != "started" {
		t.Errorf("a fire just after the first turn's cooldown, which runs from its own message: %q; want started", got)
	}
}

// The check is only for a reservation whose turn was taken. One that was not (the start failed, or was refused) is
// taken over as before, and the check is not made for a thread that started, or for the message's own redelivery.
func TestAFireTakesOverAThreadWhoseTurnWasNotQueued(t *testing.T) {
	for name, hookErr := range map[string]error{"a start that failed": errors.New("session store unavailable"), "a refused start": chat.ErrNotStarted} {
		t.Run(name, func(t *testing.T) {
			h, q := newTakeoverMarking(t, false)
			h.hookErr = hookErr
			_ = h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring))
			h.hookErr = nil
			h.clk.advance(2 * time.Minute)
			if got := h.send(t, "101.1", kindFiring); got != "started" {
				t.Fatalf("a fire past the grace: %q; want started: no turn was queued for the first thread", got)
			}
			if got := h.started(); !slices.Equal(got, []string{"100.1", "101.1"}) {
				t.Errorf("turns asked for %v; want the first, which did not start, and the second", got)
			}
			if got := q.asked(); !slices.Equal(got, []string{turnID("100.1")}) {
				t.Errorf("the queue was asked about %v; want the first thread's turn alone", got)
			}
		})
	}
}

// A reservation that is not stale yet may be a turn that is starting: it is neither checked nor taken over, and a
// started thread is never checked.
func TestTheQueuedTurnIsOnlyAskedAboutAStaleReservation(t *testing.T) {
	h, q := newTakeover(t)
	_ = h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring)) // reserved, queued, not marked
	h.clk.advance(10 * time.Second)
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Fatalf("a fire inside the grace: %q; want repeat", got)
	}
	if got := q.asked(); len(got) != 0 {
		t.Errorf("the queue was asked about %v inside the grace; want no question", got)
	}

	h2, q2 := newTakeover(t)
	_ = h2.in.Message(h2.ctx, botInbound("C1", "200.1", kindFiring)) // the mark fails once...
	if got := h2.send(t, "200.1", kindFiring); got != "dedup" {      // ...and the redelivery of the event marks the thread
		t.Fatalf("redelivery: %q; want dedup", got)
	}
	h2.clk.advance(2 * time.Minute)
	if got := h2.send(t, "201.1", kindFiring); got != "repeat" {
		t.Fatalf("a fire at a started thread: %q; want repeat", got)
	}
	if got := q2.asked(); len(got) != 0 {
		t.Errorf("the queue was asked about %v for a thread that started; want no question", got)
	}
}

// A queue that cannot answer is the event's failure: unacknowledged, with nothing taken over, so the redelivery asks
// again rather than starting a second turn on a guess.
func TestAQueueThatCannotAnswerLeavesTheEventToBeRedelivered(t *testing.T) {
	h, q := newTakeover(t)
	_ = h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring))
	h.clk.advance(2 * time.Minute)
	q.err = errors.New("disk full")
	if err := h.in.Message(h.ctx, botInbound("C1", "101.1", kindFiring)); !errors.Is(err, q.err) {
		t.Fatalf("Message = %v; want the queue's error", err)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1"}) {
		t.Errorf("turns started for %v after a failed check; want no second turn", got)
	}
	q.err = nil
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Errorf("redelivery: %q; want repeat once the queue answers", got)
	}
}

// With no way to ask, the Intake behaves as it did: the stale reservation is taken over.
func TestWithoutAQueueCheckAStaleReservationIsTakenOver(t *testing.T) {
	h, _ := newTakeover(t)
	cfg := h.cfg
	cfg.TurnQueued = nil
	h.in = chat.NewIntake(cfg)
	_ = h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring))
	h.clk.advance(2 * time.Minute)
	if got := h.send(t, "101.1", kindFiring); got != "started" {
		t.Errorf("a fire past the grace: %q; want started", got)
	}
}
