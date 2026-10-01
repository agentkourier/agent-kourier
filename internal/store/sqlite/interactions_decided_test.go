package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestListDecidedInteractions returns the interactions decided since a time, in the order they were
// decided: those a prompt may still show controls for. A pending one is not decided.
func TestListDecidedInteractions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for _, id := range []string{"pending", "old", "just-before", "at-since", "answered", "timed", "cancelled"} {
		if _, err := s.CreateInteraction(ctx, newInteraction(id, time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Hour) }
	for id, m := range map[string]int{"old": 1, "answered": 5} { // answered while before its deadline
		if err := s.SubmitInteraction(ctx, id, []byte(`{}`), "U1", t0.Add(time.Duration(m)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// The answers are decided at the time of the press, so the times the test cares about are set directly.
	if err := s.ResolveInteraction(ctx, "timed", store.InteractionTimedOut, at(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveInteraction(ctx, "cancelled", store.InteractionCancelled, at(4)); err != nil {
		t.Fatal(err)
	}
	// The boundary: one decided exactly at since is listed, and one a nanosecond before it is not.
	if err := s.ResolveInteraction(ctx, "at-since", store.InteractionCancelled, at(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveInteraction(ctx, "just-before", store.InteractionCancelled, at(2).Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE interactions SET decided_at = ? WHERE id = 'old'`, ts(at(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE interactions SET decided_at = ? WHERE id = 'answered'`, ts(at(5))); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDecidedInteractions(ctx, at(2))
	var ids []string
	for _, i := range got {
		ids = append(ids, i.ID)
	}
	if err != nil || !slices.Equal(ids, []string{"at-since", "timed", "cancelled", "answered"}) {
		t.Fatalf("ListDecidedInteractions = %v, %v; want at-since, timed, cancelled, answered: decided since (the boundary included), by when, and not pending or older", ids, err)
	}
	if got[3].State != store.InteractionAnswered || got[3].Decider != "U1" || got[1].State != store.InteractionTimedOut {
		t.Errorf("rows = %+v; want them whole", got)
	}
	if _, err := s.ListDecidedInteractions(ctx, time.Time{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("a zero time = %v; want ErrInvalid", err)
	}
}
