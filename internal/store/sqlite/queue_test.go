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

var _ store.Queue = (*Store)(nil)

const lease = 30 * time.Second

func appendAt(t *testing.T, s *Store, id string, received time.Time) {
	t.Helper()
	e := store.Event{ID: id, Kind: "alert", BindingKey: "ns/b", Payload: []byte("p"), ReceivedAt: received}
	if _, err := s.AppendEvent(context.Background(), e); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

func mustGet(t *testing.T, s *Store, id string) store.Event {
	t.Helper()
	e, err := s.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEvent(%s): %v", id, err)
	}
	return e
}

func TestClaimEventReturnsOldestDueAndSkipsFuture(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.ClaimEvent(ctx, t0, lease); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim on empty queue = %v; want ErrNotFound", err)
	}
	// Appended out of order; the future event must never be claimed at now.
	appendAt(t, s, "newer", t0.Add(2*time.Second))
	appendAt(t, s, "future", t0.Add(time.Hour))
	appendAt(t, s, "oldest", t0.Add(time.Second))
	now := t0.Add(3 * time.Second)

	for _, want := range []string{"oldest", "newer"} {
		got, err := s.ClaimEvent(ctx, now, lease)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got.ID != want || got.Status != store.EventProcessing || got.Attempts != 1 ||
			!got.NextAttemptAt.Equal(now.Add(lease)) || string(got.Payload) != "p" || got.Kind != "alert" {
			t.Fatalf("claimed %+v; want %s processing, attempts 1, lease deadline %v", got, want, now.Add(lease))
		}
		if stored := mustGet(t, s, want); stored.Status != store.EventProcessing || stored.Attempts != 1 {
			t.Fatalf("stored after claim = %+v", stored)
		}
	}
	if got, err := s.ClaimEvent(ctx, now, lease); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim with only a future event = %+v, %v; want ErrNotFound", got, err)
	}
	if got := mustGet(t, s, "future"); got.Status != store.EventPending || got.Attempts != 0 {
		t.Fatalf("future event was touched: %+v", got)
	}
}

func TestClaimEventLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := s.ClaimEvent(ctx, t0.Add(lease-time.Nanosecond), lease); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim before lease expiry = %v; want ErrNotFound", err)
	}
	later := t0.Add(lease)
	got, err := s.ClaimEvent(ctx, later, lease)
	if err != nil {
		t.Fatalf("reclaim at lease expiry: %v", err)
	}
	if got.ID != "ev" || got.Attempts != 2 || !got.NextAttemptAt.Equal(later.Add(lease)) {
		t.Fatalf("reclaimed %+v; want attempts 2 and a fresh lease", got)
	}
}

func TestClaimEventIsExclusiveUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	const events, claimers = 60, 8
	for i := range events {
		appendAt(t, s, fmt.Sprintf("ev%02d", i), t0.Add(time.Duration(i)*time.Millisecond))
	}
	now := t0.Add(time.Hour)

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for range claimers {
		wg.Go(func() {
			for {
				e, err := s.ClaimEvent(ctx, now, time.Hour)
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				claimed[e.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(claimed) != events {
		t.Fatalf("claimed %d distinct events; want %d", len(claimed), events)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("event %s claimed %d times; want 1", id, n)
		}
	}
}

func TestFailEventRetriesThenDies(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	const maxAttempts = 3
	now := t0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := s.ClaimEvent(ctx, now, lease); err != nil {
			t.Fatalf("attempt %d claim: %v", attempt, err)
		}
		retryAt := now.Add(time.Minute)
		msg := fmt.Sprintf("boom %d", attempt)
		if err := s.FailEvent(ctx, "ev", attempt, retryAt, maxAttempts, msg); err != nil {
			t.Fatalf("attempt %d fail: %v", attempt, err)
		}
		got := mustGet(t, s, "ev")
		if got.LastError != msg || got.Attempts != attempt {
			t.Fatalf("attempt %d: %+v; want last error %q", attempt, got, msg)
		}
		if attempt == maxAttempts {
			if got.Status != store.EventDead {
				t.Fatalf("attempt %d: status %q; want dead", attempt, got.Status)
			}
			break
		}
		if got.Status != store.EventPending || !got.NextAttemptAt.Equal(retryAt) {
			t.Fatalf("attempt %d: %+v; want pending at %v", attempt, got, retryAt)
		}
		if _, err := s.ClaimEvent(ctx, retryAt.Add(-time.Nanosecond), lease); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("attempt %d: claimed before retryAt: %v", attempt, err)
		}
		now = retryAt
	}
	if _, err := s.ClaimEvent(ctx, now.Add(24*time.Hour), lease); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dead event was claimable: %v", err)
	}
}

func TestCompleteEvent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.CompleteEvent(ctx, "ev", 1); err != nil {
		t.Fatalf("CompleteEvent: %v", err)
	}
	if got := mustGet(t, s, "ev"); got.Status != store.EventDone {
		t.Fatalf("status = %q; want done", got.Status)
	}
	if _, err := s.ClaimEvent(ctx, t0.Add(time.Hour), lease); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("done event was claimable: %v", err)
	}
}

// A reclaim bumps Attempts, so the earlier holder's token no longer matches.
func TestStaleHolderCannotChangeReclaimedEvent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	if _, err := s.ClaimEvent(ctx, t0, lease); err != nil { // A holds attempt 1
		t.Fatalf("claim A: %v", err)
	}
	reclaimAt := t0.Add(lease)
	b, err := s.ClaimEvent(ctx, reclaimAt, lease) // B holds attempt 2
	if err != nil || b.Attempts != 2 {
		t.Fatalf("claim B = %+v, %v; want attempt 2", b, err)
	}

	errs := map[string]error{
		"A complete": s.CompleteEvent(ctx, "ev", 1),
		"A fail":     s.FailEvent(ctx, "ev", 1, t0, 1, "late"),
	}
	for name, err := range errs {
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("%s = %v; want ErrConflict", name, err)
		} else if want := "lease lost: event is processing at attempt 2, caller holds 1"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %q; want it to contain %q", name, err, want)
		}
	}
	if got := mustGet(t, s, "ev"); got.Status != store.EventProcessing || got.Attempts != 2 ||
		got.LastError != "" || !got.NextAttemptAt.Equal(reclaimAt.Add(lease)) {
		t.Fatalf("stale holder disturbed B's lease: %+v", got)
	}
	if err := s.CompleteEvent(ctx, "ev", 2); err != nil {
		t.Fatalf("B complete: %v", err)
	}
}

// The fence rejects lost claims, not late ones.
func TestExpiredButUnclaimedLeaseCanStillComplete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.CompleteEvent(ctx, "ev", 1); err != nil {
		t.Fatalf("complete after lease expiry without reclaim: %v", err)
	}
}

// The store enforces no attempt limit, so crashes can push Attempts past the
// worker's maximum; FailEvent must still dead-letter such an event.
func TestFailEventDeadLettersClaimPastMaxAttempts(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "ev", t0)
	const maxAttempts = 2
	var e store.Event
	for i := range 3 { // three crashed workers: each claim expires unreleased
		var err error
		if e, err = s.ClaimEvent(ctx, t0.Add(time.Duration(i)*lease), lease); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	if e.Attempts != 3 {
		t.Fatalf("attempts = %d; want 3, past max %d", e.Attempts, maxAttempts)
	}
	if err := s.FailEvent(ctx, "ev", e.Attempts, t0, maxAttempts, "too many attempts"); err != nil {
		t.Fatalf("FailEvent: %v", err)
	}
	if got := mustGet(t, s, "ev"); got.Status != store.EventDead || got.LastError != "too many attempts" ||
		!got.NextAttemptAt.Equal(e.NextAttemptAt) {
		t.Fatalf("after fail = %+v; want dead", got)
	}
}

func TestCompleteAndFailRequireProcessing(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	appendAt(t, s, "pending", t0)
	appendAt(t, s, "done", t0)
	appendAt(t, s, "dead", t0)
	for id, st := range map[string]store.EventStatus{"done": store.EventDone, "dead": store.EventDead} {
		if err := s.SetEventStatus(ctx, id, st, ""); err != nil {
			t.Fatalf("SetEventStatus(%s): %v", id, err)
		}
	}
	for _, id := range []string{"pending", "done", "dead"} {
		if err := s.CompleteEvent(ctx, id, 1); !errors.Is(err, store.ErrConflict) {
			t.Errorf("CompleteEvent(%s) = %v; want ErrConflict", id, err)
		}
		if err := s.FailEvent(ctx, id, 1, t0, 3, "x"); !errors.Is(err, store.ErrConflict) {
			t.Errorf("FailEvent(%s) = %v; want ErrConflict", id, err)
		}
	}
	if got := mustGet(t, s, "pending"); got.Status != store.EventPending || got.LastError != "" {
		t.Errorf("rejected FailEvent modified the event: %+v", got)
	}
	if err := s.CompleteEvent(ctx, "missing", 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CompleteEvent(missing) = %v; want ErrNotFound", err)
	}
	if err := s.FailEvent(ctx, "missing", 1, t0, 3, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FailEvent(missing) = %v; want ErrNotFound", err)
	}
}

func TestQueueRejectsInvalidArguments(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	claim := func(now time.Time, l time.Duration) error { _, err := s.ClaimEvent(ctx, now, l); return err }
	checks := map[string]error{
		"claim zero now":     claim(time.Time{}, lease),
		"claim zero lease":   claim(t0, 0),
		"claim neg lease":    claim(t0, -time.Second),
		"complete empty id":  s.CompleteEvent(ctx, "", 1),
		"complete attempt 0": s.CompleteEvent(ctx, "ev", 0),
		"fail empty id":      s.FailEvent(ctx, "", 1, t0, 3, "x"),
		"fail attempt 0":     s.FailEvent(ctx, "ev", 0, t0, 3, "x"),
		"fail zero retry":    s.FailEvent(ctx, "ev", 1, time.Time{}, 3, "x"),
		"fail zero max":      s.FailEvent(ctx, "ev", 1, t0, 0, "x"),
	}
	for name, err := range checks {
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s = %v; want ErrInvalid", name, err)
		}
	}
}
