package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func createInteraction(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.CreateInteraction(context.Background(), newInteraction(id, time.Hour)); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func TestSubmitInteraction(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	now := t0.Add(time.Minute)
	createInteraction(t, s, "i1")

	if err := s.SubmitInteraction(ctx, "i1", []byte("yes"), "U1", now); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	got, err := s.GetInteraction(ctx, "i1")
	if err != nil || got.State != store.InteractionAnswered || string(got.Answer) != "yes" || got.Decider != "U1" || !got.DecidedAt.Equal(now) {
		t.Fatalf("after submit = %+v, %v", got, err)
	}
	err = s.SubmitInteraction(ctx, "i1", []byte("no"), "U2", now)
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "U1") {
		t.Fatalf("second submit = %v; want ErrConflict naming U1", err)
	}
	if got, _ = s.GetInteraction(ctx, "i1"); string(got.Answer) != "yes" || got.Decider != "U1" {
		t.Fatalf("second submit changed the answer: %+v", got)
	}

	// At the deadline, or after the sweep timed it out, a submit is expired and changes nothing.
	createInteraction(t, s, "i2")
	createInteraction(t, s, "i3")
	if err := s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, c := range []struct{ id, wantState string }{{"i2", "pending"}, {"i3", "timed_out"}} {
		if err := s.SubmitInteraction(ctx, c.id, []byte("late"), "U1", t0.Add(time.Hour)); !errors.Is(err, store.ErrExpired) {
			t.Errorf("%s: submit at the deadline = %v; want ErrExpired", c.id, err)
		}
		if got, _ = s.GetInteraction(ctx, c.id); string(got.State) != c.wantState || got.Decider != "" {
			t.Errorf("%s: expired submit changed the row: %+v", c.id, got)
		}
	}
	if err := s.SubmitInteraction(ctx, "nope", []byte("x"), "U1", now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("submit missing = %v; want ErrNotFound", err)
	}
	for _, bad := range []func() error{
		func() error { return s.SubmitInteraction(ctx, "", []byte("x"), "U1", now) },
		func() error { return s.SubmitInteraction(ctx, "i2", nil, "U1", now) },
		func() error { return s.SubmitInteraction(ctx, "i2", []byte("x"), "", now) },
		func() error { return s.SubmitInteraction(ctx, "i2", []byte("x"), "U1", time.Time{}) },
	} {
		if err := bad(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("invalid submit = %v; want ErrInvalid", err)
		}
	}
}

func TestSubmitInteractionConcurrentHasOneWinner(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	createInteraction(t, s, "i1")
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs[i] = s.SubmitInteraction(ctx, "i1", []byte("a"), fmt.Sprintf("U%d", i), t0.Add(time.Minute))
		})
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, store.ErrConflict) {
			t.Errorf("submit %d = %v; want nil or ErrConflict", i, err)
		}
	}
	got, err := s.GetInteraction(ctx, "i1")
	if winners != 1 || err != nil || got.Decider == "" {
		t.Fatalf("winners = %d, row = %+v, %v; want exactly one", winners, got, err)
	}
	for _, e := range errs {
		if e != nil && !strings.Contains(e.Error(), got.Decider) {
			t.Errorf("conflict %q does not name the winner %s", e, got.Decider)
		}
	}
}

func TestResolveInteractionOnlyFromPending(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	now := t0.Add(time.Minute)
	for _, id := range []string{"i1", "i2", "i3"} {
		createInteraction(t, s, id)
	}
	if err := s.ResolveInteraction(ctx, "i1", store.InteractionCancelled, now); err != nil {
		t.Fatalf("resolve pending: %v", err)
	}
	if got, err := s.GetInteraction(ctx, "i1"); err != nil || got.State != store.InteractionCancelled || !got.DecidedAt.Equal(now) || got.Decider != "" {
		t.Fatalf("after resolve = %+v, %v", got, err)
	}
	if err := s.SubmitInteraction(ctx, "i2", []byte("yes"), "U1", now); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// A timeout before the deadline is refused, so the answer window stays open; at the deadline it works.
	if err := s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour-1)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("early timeout = %v; want ErrConflict", err)
	}
	if got, _ := s.GetInteraction(ctx, "i3"); got.State != store.InteractionPending {
		t.Errorf("early timeout moved the row to %s", got.State)
	}
	if err := s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour)); err != nil {
		t.Errorf("timeout at the deadline: %v", err)
	}
	for _, id := range []string{"i1", "i2"} { // cancelled and answered rows must not move
		if err := s.ResolveInteraction(ctx, id, store.InteractionTimedOut, now); !errors.Is(err, store.ErrConflict) {
			t.Errorf("resolve %s = %v; want ErrConflict", id, err)
		}
	}
	if got, _ := s.GetInteraction(ctx, "i2"); got.State != store.InteractionAnswered {
		t.Errorf("resolve moved an answered row to %s", got.State)
	}
	// A cancelled interaction refuses a later answer, without naming a decider.
	if err := s.SubmitInteraction(ctx, "i1", []byte("x"), "U1", now); !errors.Is(err, store.ErrConflict) {
		t.Errorf("submit to cancelled = %v; want ErrConflict", err)
	}
	if err := s.ResolveInteraction(ctx, "nope", store.InteractionTimedOut, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("resolve missing = %v; want ErrNotFound", err)
	}
	for _, st := range []store.InteractionState{store.InteractionPending, store.InteractionAnswered, "bogus"} {
		if err := s.ResolveInteraction(ctx, "i2", st, now); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("resolve to %q = %v; want ErrInvalid", st, err)
		}
	}
	if err := s.ResolveInteraction(ctx, "i2", store.InteractionTimedOut, time.Time{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("resolve with zero now = %v; want ErrInvalid", err)
	}
}

func TestSetInteractionMessageRef(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	createInteraction(t, s, "i1")
	createInteraction(t, s, "i2")
	set := func(id, ref string) error { return s.SetInteractionMessageRef(ctx, id, ref) }
	if err := set("i1", "1700.01"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := set("i1", "1700.01"); err != nil {
		t.Errorf("same ref again = %v; want no-op", err)
	}
	if err := set("i1", "1700.99"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("different ref = %v; want ErrConflict", err)
	}
	if err := set("nope", "1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing = %v; want ErrNotFound", err)
	}
	if err := set("", "1"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty id = %v; want ErrInvalid", err)
	}
	if err := set("i1", ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty ref = %v; want ErrInvalid", err)
	}
	// An answer can land before the ref is recorded; the ref is still accepted.
	if err := s.SubmitInteraction(ctx, "i2", []byte("yes"), "U1", t0.Add(time.Minute)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := set("i2", "1700.02"); err != nil {
		t.Fatalf("set after answered: %v", err)
	}
	if got, err := s.GetInteraction(ctx, "i2"); err != nil || got.MessageRef != "1700.02" || got.State != store.InteractionAnswered {
		t.Fatalf("after set = %+v, %v", got, err)
	}
}
