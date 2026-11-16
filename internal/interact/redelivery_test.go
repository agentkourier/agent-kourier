package interact_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// delivered is i as the platform delivers it, with the delivery key key.
func delivered(i chat.Interaction, key string) chat.Interaction {
	i.Delivery = key
	return i
}

// processAll processes every press waiting, oldest first, as the worker does.
func (r *pressRig) processAll(t *testing.T) {
	t.Helper()
	presses, err := r.st.ListPresses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range presses {
		if res, err := r.pl.Process(t.Context(), p); err != nil || res != interact.Done {
			t.Fatalf("Process = %v, %v", res, err)
		}
	}
}

// TestAWinnersPressDeliveredAgainIsDecidedAndSentOnce: a press that the platform delivers again, before or after the first
// delivery is processed, is acknowledged and not recorded again, so the answer is decided once and sent once: the decider never
// sees the second delivery as the winner's retry, which would send the recorded answer again.
func TestAWinnersPressDeliveredAgainIsDecidedAndSentOnce(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	i := delivered(r.submit(t, "UALICE1", 0), "delivery-1")
	for range 2 { // the second delivery comes before the first is processed
		if err := r.pl.Press(t.Context(), i); err != nil {
			t.Fatalf("Press = %v; want the delivery acknowledged", err)
		}
	}
	// The waiting press is handed over again, as it was recorded: the worker ignores a seq it holds.
	if n := r.waiting(t); n != 1 || len(r.got) != 2 || r.got[1].Seq != r.got[0].Seq || !r.got[1].PressedAt.Equal(r.got[0].PressedAt) {
		t.Fatalf("%d presses waiting, the worker told of %+v; want one waiting, handed over twice as the same press", n, r.got)
	}
	r.processAll(t)
	if err := r.pl.Press(t.Context(), i); err != nil { // and another after it is processed
		t.Fatalf("Press after the press was processed = %v; want the delivery acknowledged", err)
	}
	if n := r.waiting(t); n != 0 || len(r.got) != 2 {
		t.Fatalf("%d presses waiting, the worker told of %d; want none waiting, and nothing more handed over once the press is processed", n, len(r.got))
	}
	if row := r.row(t); row.State != store.InteractionAnswered || row.Decider != "UALICE1" || len(r.sess.answers()) != 1 || len(r.aud.got) != 1 || len(r.eph.list()) != 0 {
		t.Errorf("state %s by %s, %d sends, %d audit rows, told %q; want answered by UALICE1, sent and audited once, nobody told", row.State, row.Decider, len(r.sess.answers()), len(r.aud.got), r.eph.list())
	}
}

// TestALosersPressDeliveredAgainIsToldOnce: the person whose press lost is told who decided first once, however many times the
// platform delivers the press.
func TestALosersPressDeliveredAgainIsToldOnce(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	if err := r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{"request_id":"req-1","answers":[["a"]]}`), "UALICE1", d0); err != nil {
		t.Fatal(err)
	}
	i := delivered(r.submit(t, "UBOB22", 0), "delivery-2")
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	r.processAll(t)
	for range 2 {
		if err := r.pl.Press(t.Context(), i); err != nil {
			t.Fatalf("Press = %v; want the delivery acknowledged", err)
		}
		r.processAll(t)
	}
	if got := r.eph.list(); len(got) != 1 || got[0] != "UBOB22: @UALICE1 answered this question first." || len(r.got) != 1 {
		t.Errorf("told %q, the worker told of %d presses; want UBOB22 told once who answered first, and one press", got, len(r.got))
	}
}

// TestAPressDeliveredAgainIsNotRefusedByTheLimits: a delivery of a press already recorded is acknowledged even when the person
// has as many presses waiting as the limit allows, since it takes no place; a new press is still refused.
func TestAPressDeliveredAgainIsNotRefusedByTheLimits(t *testing.T) {
	r := newPressRig(t) // two presses of one person may wait
	r.open(t)
	first, second := delivered(r.submit(t, "UALICE1", 0), "delivery-1"), delivered(r.submit(t, "UALICE1", 1), "delivery-2")
	for _, i := range []chat.Interaction{first, second, first} {
		if err := r.pl.Press(t.Context(), i); err != nil {
			t.Fatalf("Press of %s = %v; want it acknowledged", i.Delivery, err)
		}
	}
	if err := r.pl.Press(t.Context(), delivered(r.submit(t, "UALICE1", 0), "delivery-3")); !errors.Is(err, store.ErrLimit) {
		t.Errorf("a third press = %v; want store.ErrLimit", err)
	}
	if n := r.waiting(t); n != 2 || len(r.got) != 3 || r.got[2].Seq != r.got[0].Seq {
		t.Errorf("%d presses waiting, the worker told of %+v; want 2 waiting, and the first handed over again, which a worker ignores", n, r.got)
	}
}

// ambiguousCommit is a store whose first RecordPress commits and then returns an error, as a Postgres COMMIT whose answer the
// network lost does.
type ambiguousCommit struct {
	interact.PressStore
	failed bool
}

func (s *ambiguousCommit) RecordPress(ctx context.Context, p store.Press, limits store.PressLimits) (int64, error) {
	seq, err := s.PressStore.RecordPress(ctx, p, limits)
	if err == nil && !s.failed {
		s.failed = true
		return 0, errors.New("connection reset by peer")
	}
	return seq, err
}

// TestAPressWhoseRecordFailedAfterItsCommitIsProcessedWhenDeliveredAgain: the first delivery's record committed and returned an
// error, so the delivery was left unacknowledged and the worker was not told. The platform's second delivery hands the worker
// the press as it was recorded, at its own time, and it is decided once.
func TestAPressWhoseRecordFailedAfterItsCommitIsProcessedWhenDeliveredAgain(t *testing.T) {
	r := newPressRig(t)
	r.open(t)
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
	var err error
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{Store: &ambiguousCommit{PressStore: r.st}, Sessions: r.st, Lookup: lookup{b},
		Authorizers: chattest.Same[interact.Authorizer](r.az), Ephemerals: chattest.Same[interact.Ephemeral](r.eph), Decider: r.d, Auditor: r.aud,
		Limits: store.PressLimits{Total: 10, PerUser: 2}, Logger: testutil.DiscardLogger(),
		Now: func() time.Time { return r.now }, Notify: func(p store.Press) { r.got = append(r.got, p) }}))
	if err != nil {
		t.Fatal(err)
	}
	i := delivered(r.submit(t, "UALICE1", 0), "delivery-1")
	pressed := r.now
	if err := r.pl.Press(t.Context(), i); err == nil {
		t.Fatal("Press = nil; want the record's error, which leaves the delivery unacknowledged")
	}
	if n := r.waiting(t); n != 1 || len(r.got) != 0 {
		t.Fatalf("%d presses waiting, the worker told of %d; want the committed press waiting and the worker untold", n, len(r.got))
	}
	r.now = r.now.Add(time.Minute) // the platform's retry comes later
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatalf("Press of the delivery again = %v; want it acknowledged", err)
	}
	if len(r.got) != 1 || r.got[0].Seq == 0 || !r.got[0].PressedAt.Equal(pressed) || r.waiting(t) != 1 {
		t.Fatalf("the worker was told of %+v, %d presses waiting; want the one press, recorded at %v", r.got, r.waiting(t), pressed)
	}
	if res, err := r.pl.Process(t.Context(), r.got[0]); err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v", res, err)
	}
	if row := r.row(t); row.State != store.InteractionAnswered || !row.DecidedAt.Equal(pressed) || len(r.sess.answers()) != 1 || len(r.aud.got) != 1 {
		t.Errorf("state %s decided at %v, %d sends, %d audit rows; want answered at the press's own time, sent and audited once", row.State, row.DecidedAt, len(r.sess.answers()), len(r.aud.got))
	}
}
