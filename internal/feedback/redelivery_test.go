package feedback_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestAVoteDeliveredAgainIsRecordedOnce: a vote that the platform delivers again after the person has changed it is
// acknowledged and not recorded again, so it does not put back the verdict the person replaced.
func TestAVoteDeliveredAgainIsRecordedOnce(t *testing.T) {
	r := newRig(t)
	useful := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
	useful.Delivery = "delivery-1"
	notUseful := press("UALICE1", store.FeedbackInvestigation, store.VerdictNotUseful)
	notUseful.Delivery = "delivery-2"
	if _, err := r.vote(t, useful); err != nil {
		t.Fatal(err)
	}
	if _, err := r.vote(t, notUseful); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Press(t.Context(), useful); err != nil {
		t.Fatalf("Press of the first vote delivered again = %v; want it acknowledged", err)
	}
	if n := r.waiting(t); n != 0 || len(r.seen) != 2 {
		t.Errorf("%d presses waiting, the worker told of %d; want none waiting, and the two votes alone told", n, len(r.seen))
	}
	if got := r.votes(t); len(got) != 1 || got[0].Verdict != store.VerdictNotUseful {
		t.Errorf("votes = %+v; want the later verdict, not_useful, kept", got)
	}
}
