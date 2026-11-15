package interact_test

import (
	"errors"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
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
	if n := r.waiting(t); n != 1 || len(r.got) != 1 {
		t.Fatalf("%d presses waiting, the worker told of %d; want one of each", n, len(r.got))
	}
	r.processAll(t)
	if err := r.pl.Press(t.Context(), i); err != nil { // and another after it is processed
		t.Fatalf("Press after the press was processed = %v; want the delivery acknowledged", err)
	}
	if n := r.waiting(t); n != 0 || len(r.got) != 1 {
		t.Fatalf("%d presses waiting, the worker told of %d; want none waiting, and the first delivery alone told", n, len(r.got))
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
	if n := r.waiting(t); n != 2 || len(r.got) != 2 {
		t.Errorf("%d presses waiting, the worker told of %d; want 2 and 2", n, len(r.got))
	}
}
