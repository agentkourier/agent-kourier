package feedback_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
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

// ambiguousCommit is a store whose first RecordPress commits and then returns an error, as a Postgres COMMIT whose answer the
// network lost does.
type ambiguousCommit struct {
	feedback.Store
	failed bool
}

func (s *ambiguousCommit) RecordPress(ctx context.Context, p store.Press, limits store.PressLimits) (int64, error) {
	seq, err := s.Store.RecordPress(ctx, p, limits)
	if err == nil && !s.failed {
		s.failed = true
		return 0, errors.New("connection reset by peer")
	}
	return seq, err
}

// TestAVoteWhoseRecordFailedAfterItsCommitIsProcessedWhenDeliveredAgain: the first delivery's record committed and returned an
// error, so the delivery was left unacknowledged and the worker was not told. The platform's second delivery hands the worker
// the vote as it was recorded, at its own time, and it is counted once; a delivery after it is processed hands over nothing.
func TestAVoteWhoseRecordFailedAfterItsCommitIsProcessedWhenDeliveredAgain(t *testing.T) {
	r := newRig(t)
	var err error
	r.h, err = feedback.New(feedback.Config{
		Store: &ambiguousCommit{Store: r.st}, Sessions: r.st, Authorizers: chattest.Same[interact.Authorizer](r.az), Ephemerals: chattest.Same[interact.Ephemeral](r.eph), Auditor: r.aud,
		Limits: store.PressLimits{Total: 10, PerUser: 3}, Logger: slog.New(slog.NewTextHandler(&r.logs, nil)),
		Now: func() time.Time { return r.now }, Notify: func(p store.Press) { r.seen = append(r.seen, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	useful := press("UALICE1", store.FeedbackInvestigation, store.VerdictUseful)
	useful.Delivery = "delivery-1"
	if err := r.h.Press(t.Context(), useful); err == nil {
		t.Fatal("Press whose record returned an error = nil; want the error, so that the delivery is not acknowledged")
	}
	if n := r.waiting(t); n != 1 || len(r.seen) != 0 {
		t.Fatalf("%d presses waiting, the worker told of %d; want the vote recorded and the worker not told", n, len(r.seen))
	}
	r.now = t0.Add(time.Minute) // the platform's second delivery comes later
	if err := r.h.Press(t.Context(), useful); err != nil {
		t.Fatalf("Press of the vote delivered again = %v; want it acknowledged", err)
	}
	if len(r.seen) != 1 || r.seen[0].Seq == 0 || !r.seen[0].PressedAt.Equal(t0) {
		t.Fatalf("the worker was told of %+v; want the recorded vote, at the time of its first delivery", r.seen)
	}
	if _, err := r.h.Process(t.Context(), r.seen[0]); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Press(t.Context(), useful); err != nil {
		t.Fatalf("Press of the vote delivered a third time = %v; want it acknowledged", err)
	}
	if got := r.votes(t); len(got) != 1 || got[0].Verdict != store.VerdictUseful || len(r.seen) != 1 || r.waiting(t) != 0 {
		t.Errorf("votes %+v, the worker told of %d, %d waiting; want the one vote counted, and the third delivery handing over nothing", got, len(r.seen), r.waiting(t))
	}
}
