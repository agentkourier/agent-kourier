package storetest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func newEvent(id string, received time.Time) store.Event {
	return store.Event{ID: id, Kind: "alert", BindingKey: "ns/b", Payload: []byte("p"), ReceivedAt: received}
}

func appendEvent(t *testing.T, s store.Store, id string, received time.Time) {
	t.Helper()
	if _, err := s.AppendEvent(t.Context(), newEvent(id, received)); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

func getEvent(t *testing.T, s store.Store, id string) store.Event {
	t.Helper()
	e, err := s.GetEvent(t.Context(), id)
	if err != nil {
		t.Fatalf("GetEvent(%s): %v", id, err)
	}
	return e
}

func (su suite) events(t *testing.T) {
	su.run(t, "an event is stored once by its ID and read back whole", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		payload := make([]byte, 256) // every byte value, so the payload is opaque bytes and not text
		for i := range payload {
			payload[i] = byte(i)
		}
		e := store.Event{ID: "ev1", Kind: "alert", BindingKey: "ns/b", Payload: payload, ReceivedAt: t0}

		for _, bad := range []store.Event{{ReceivedAt: t0}, {ID: "x"}} {
			_, err := s.AppendEvent(ctx, bad)
			isErr(t, err, store.ErrInvalid, "AppendEvent of an incomplete event")
		}
		if ins, err := s.AppendEvent(ctx, e); err != nil || !ins {
			t.Fatalf("first append = %v, %v; want true, nil", ins, err)
		}
		dup := e
		dup.Payload = []byte("changed")
		if ins, err := s.AppendEvent(ctx, dup); err != nil || ins {
			t.Fatalf("duplicate append = %v, %v; want false, nil", ins, err)
		}
		got := getEvent(t, s, "ev1")
		if !bytes.Equal(got.Payload, payload) || got.Kind != "alert" || got.BindingKey != "ns/b" ||
			!got.ReceivedAt.Equal(t0) || got.Status != store.EventPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(t0) || got.LastError != "" {
			t.Fatalf("stored event = %+v; want the first append, pending, never attempted, due at its arrival", got)
		}

		_, err := s.GetEvent(ctx, "nope")
		isErr(t, err, store.ErrNotFound, "GetEvent of a missing event")
	})

	// AppendEvent documents that it ignores the fields the queue owns.
	su.run(t, "an appended event starts pending whatever the caller put in the fields the queue owns", func(t *testing.T, s store.Store) {
		e := newEvent("ev1", t0)
		e.Status, e.Attempts, e.NextAttemptAt, e.LastError = store.EventDone, 5, t0.Add(time.Hour), "boom"
		if _, err := s.AppendEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		got := getEvent(t, s, "ev1")
		if got.Status != store.EventPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(t0) || got.LastError != "" {
			t.Errorf("stored event = %+v; want pending, attempts 0, due at %v, no error", got, t0)
		}
		if c, err := s.ClaimEvent(t.Context(), t0, time.Minute); err != nil || c.ID != "ev1" {
			t.Errorf("claim at its arrival = %+v, %v; want the event", c, err)
		}
	})

	// Implicit in the interface: an event with no payload, or a nil one, is stored with an empty one.
	su.run(t, "an event with no payload comes back with an empty one", func(t *testing.T, s store.Store) {
		e := newEvent("ev1", t0)
		e.Payload = nil
		if _, err := s.AppendEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		if got := getEvent(t, s, "ev1"); len(got.Payload) != 0 {
			t.Errorf("payload = %q; want empty", got.Payload)
		}
	})

	su.run(t, "SetEventStatus changes the status and the last error and nothing else", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev1", t0)
		claimed, err := s.ClaimEvent(ctx, t0, time.Minute)
		noErr(t, err, "claim")
		if err := s.SetEventStatus(ctx, "ev1", store.EventDead, "boom"); err != nil {
			t.Fatalf("SetEventStatus: %v", err)
		}
		got := getEvent(t, s, "ev1")
		if got.Status != store.EventDead || got.LastError != "boom" || got.Attempts != claimed.Attempts || !got.NextAttemptAt.Equal(claimed.NextAttemptAt) {
			t.Fatalf("after status update = %+v; want dead with the error and the claim's attempts and lease deadline kept", got)
		}
		if err := s.SetEventStatus(ctx, "ev1", store.EventFailed, ""); err != nil {
			t.Fatal(err)
		}
		if got = getEvent(t, s, "ev1"); got.Status != store.EventFailed || got.LastError != "" {
			t.Errorf("after clearing the error = %+v", got)
		}
		isErr(t, s.SetEventStatus(ctx, "ev1", "bogus", ""), store.ErrInvalid, "SetEventStatus to an undefined status")
		isErr(t, s.SetEventStatus(ctx, "nope", store.EventDone, ""), store.ErrNotFound, "SetEventStatus of a missing event")
	})

	su.run(t, "many appends of one ID insert it exactly once", func(t *testing.T, s store.Store) {
		const n = 16
		inserted := make([]bool, n)
		parallel(n, func(i int) {
			var err error
			if inserted[i], err = s.AppendEvent(t.Context(), newEvent("same", t0)); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		})
		wins := 0
		for _, ok := range inserted {
			if ok {
				wins++
			}
		}
		if wins != 1 {
			t.Errorf("%d appends reported the insert; want exactly 1", wins)
		}
		if c, err := s.ClaimEvent(t.Context(), t0, time.Minute); err != nil || c.ID != "same" {
			t.Fatalf("claim = %+v, %v", c, err)
		}
		_, err := s.ClaimEvent(t.Context(), t0, time.Minute)
		isErr(t, err, store.ErrNotFound, "a second claim: the event is stored once")
	})
}

func (su suite) queue(t *testing.T) {
	const lease = 30 * time.Second

	su.run(t, "a claim takes the oldest due event and skips one that is not due", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.ClaimEvent(ctx, t0, lease)
		isErr(t, err, store.ErrNotFound, "claim on an empty queue")
		// Appended out of order; the future event must never be claimed at now.
		appendEvent(t, s, "newer", t0.Add(2*time.Second))
		appendEvent(t, s, "future", t0.Add(time.Hour))
		appendEvent(t, s, "oldest", t0.Add(time.Second))
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
			if stored := getEvent(t, s, want); stored.Status != store.EventProcessing || stored.Attempts != 1 {
				t.Fatalf("stored after claim = %+v", stored)
			}
		}
		_, err = s.ClaimEvent(ctx, now, lease)
		isErr(t, err, store.ErrNotFound, "claim with only a future event")
		if got := getEvent(t, s, "future"); got.Status != store.EventPending || got.Attempts != 0 {
			t.Fatalf("future event was touched: %+v", got)
		}
	})

	// Implicit in the interface, which says only "the oldest claimable event": the order is by when an
	// event is due (its retry time, or its lease deadline), then by arrival.
	su.run(t, "claims go by when an event is due, then by arrival", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "first-in", t0)
		appendEvent(t, s, "second-in", t0.Add(time.Minute))
		if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
			t.Fatal(err)
		}
		// first-in fails and is due again in ten minutes, after second-in, which arrived later.
		if err := s.FailEvent(ctx, "first-in", 1, t0.Add(10*time.Minute), 3, "retry"); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"second-in", "first-in"} {
			got, err := s.ClaimEvent(ctx, t0.Add(time.Hour), lease)
			if err != nil || got.ID != want {
				t.Fatalf("claim = %+v, %v; want %s", got, err, want)
			}
		}
	})

	// The interface says "the oldest claimable event" and today's order is by when an event is due, then by
	// arrival; a tie on the due time goes to the event that arrived first.
	su.run(t, "events due at the same time are claimed in the order they arrived", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "a", t0)
		appendEvent(t, s, "b", t0.Add(time.Minute))
		retryAt := t0.Add(time.Hour)
		for range 2 { // both are claimed, then fail in the opposite order of their arrival
			if _, err := s.ClaimEvent(ctx, t0.Add(2*time.Minute), lease); err != nil {
				t.Fatal(err)
			}
		}
		noErr(t, s.FailEvent(ctx, "b", 1, retryAt, 3, "x"), "FailEvent b")
		noErr(t, s.FailEvent(ctx, "a", 1, retryAt, 3, "x"), "FailEvent a")
		for _, want := range []string{"a", "b"} {
			got, err := s.ClaimEvent(ctx, retryAt, lease)
			if err != nil || got.ID != want {
				t.Fatalf("claim = %+v, %v; want %s", got, err, want)
			}
		}
	})

	su.run(t, "a claim is not visible to a second claimer until its lease expires", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
			t.Fatalf("first claim: %v", err)
		}
		_, err := s.ClaimEvent(ctx, t0, lease)
		isErr(t, err, store.ErrNotFound, "second claim at the same time")
		_, err = s.ClaimEvent(ctx, t0.Add(lease-time.Microsecond), lease)
		isErr(t, err, store.ErrNotFound, "claim before lease expiry")
		later := t0.Add(lease)
		got, err := s.ClaimEvent(ctx, later, lease)
		if err != nil {
			t.Fatalf("reclaim at lease expiry: %v", err)
		}
		if got.ID != "ev" || got.Attempts != 2 || !got.NextAttemptAt.Equal(later.Add(lease)) {
			t.Fatalf("reclaimed %+v; want attempts 2 and a fresh lease", got)
		}
	})

	su.run(t, "concurrent claimers each get a different event", func(t *testing.T, s store.Store) {
		const events, claimers = 60, 8
		for i := range events {
			appendEvent(t, s, "ev"+string(rune('A'+i/26))+string(rune('a'+i%26)), t0.Add(time.Duration(i)*time.Millisecond))
		}
		now := t0.Add(time.Hour)
		// Each claimer stops after events+1 claims, so a store that hands out an event twice ends the
		// test with a message and not a hang.
		claimed := make([][]string, claimers)
		parallel(claimers, func(c int) {
			for range events + 1 {
				e, err := s.ClaimEvent(t.Context(), now, time.Hour)
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				claimed[c] = append(claimed[c], e.ID)
			}
		})
		seen := map[string]int{}
		for _, ids := range claimed {
			for _, id := range ids {
				seen[id]++
			}
		}
		if len(seen) != events {
			t.Errorf("claimed %d distinct events; want %d", len(seen), events)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("event %s claimed %d times; want 1", id, n)
			}
		}
	})

	su.run(t, "of many claimers of an expired lease exactly one takes it over", func(t *testing.T, s store.Store) {
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(t.Context(), t0, lease); err != nil {
			t.Fatal(err)
		}
		now := t0.Add(lease)
		const claimers = 8
		got := make([]store.Event, claimers)
		errs := make([]error, claimers)
		parallel(claimers, func(i int) { got[i], errs[i] = s.ClaimEvent(t.Context(), now, lease) })
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				if got[i].Attempts != 2 {
					t.Errorf("the winner holds attempt %d; want 2", got[i].Attempts)
				}
			case !errors.Is(err, store.ErrNotFound):
				t.Errorf("claimer %d = %v; want nil or ErrNotFound", i, err)
			}
		}
		if winners != 1 {
			t.Errorf("%d claimers took the lease over; want exactly 1", winners)
		}
	})

	su.run(t, "a failed event is retried at the time given, then dead-lettered at the limit", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
		const maxAttempts = 3
		now := t0
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if _, err := s.ClaimEvent(ctx, now, lease); err != nil {
				t.Fatalf("attempt %d claim: %v", attempt, err)
			}
			retryAt := now.Add(time.Minute)
			msg := "boom " + string(rune('0'+attempt))
			if err := s.FailEvent(ctx, "ev", attempt, retryAt, maxAttempts, msg); err != nil {
				t.Fatalf("attempt %d fail: %v", attempt, err)
			}
			got := getEvent(t, s, "ev")
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
			_, err := s.ClaimEvent(ctx, retryAt.Add(-time.Microsecond), lease)
			isErr(t, err, store.ErrNotFound, "claim before retryAt")
			now = retryAt
		}
		_, err := s.ClaimEvent(ctx, now.Add(24*time.Hour), lease)
		isErr(t, err, store.ErrNotFound, "claim of a dead event")
	})

	su.run(t, "a completed event is done and never claimed again", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(ctx, t0, lease); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.CompleteEvent(ctx, "ev", 1); err != nil {
			t.Fatalf("CompleteEvent: %v", err)
		}
		if got := getEvent(t, s, "ev"); got.Status != store.EventDone {
			t.Fatalf("status = %q; want done", got.Status)
		}
		_, err := s.ClaimEvent(ctx, t0.Add(time.Hour), lease)
		isErr(t, err, store.ErrNotFound, "claim of a done event")
	})

	// A reclaim bumps Attempts, so the earlier holder's token no longer matches.
	su.run(t, "a holder who lost the claim cannot change the event", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(ctx, t0, lease); err != nil { // A holds attempt 1
			t.Fatalf("claim A: %v", err)
		}
		reclaimAt := t0.Add(lease)
		b, err := s.ClaimEvent(ctx, reclaimAt, lease) // B holds attempt 2
		if err != nil || b.Attempts != 2 {
			t.Fatalf("claim B = %+v, %v; want attempt 2", b, err)
		}
		isErr(t, s.CompleteEvent(ctx, "ev", 1), store.ErrConflict, "A complete")
		isErr(t, s.FailEvent(ctx, "ev", 1, t0, 1, "late"), store.ErrConflict, "A fail")
		if got := getEvent(t, s, "ev"); got.Status != store.EventProcessing || got.Attempts != 2 ||
			got.LastError != "" || !got.NextAttemptAt.Equal(reclaimAt.Add(lease)) {
			t.Fatalf("stale holder disturbed B's lease: %+v", got)
		}
		if err := s.CompleteEvent(ctx, "ev", 2); err != nil {
			t.Fatalf("B complete: %v", err)
		}
	})

	// The fence rejects lost claims, not late ones.
	su.run(t, "a lease that expired but was never reclaimed can still complete", func(t *testing.T, s store.Store) {
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(t.Context(), t0, lease); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.CompleteEvent(t.Context(), "ev", 1); err != nil {
			t.Fatalf("complete after lease expiry without reclaim: %v", err)
		}
	})

	// The store enforces no attempt limit, so crashes can push Attempts past the worker's maximum;
	// FailEvent must still dead-letter such an event.
	su.run(t, "an event claimed past the attempt limit is dead-lettered by FailEvent", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
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
		if got := getEvent(t, s, "ev"); got.Status != store.EventDead || got.LastError != "too many attempts" ||
			!got.NextAttemptAt.Equal(e.NextAttemptAt) {
			t.Fatalf("after fail = %+v; want dead", got)
		}
	})

	su.run(t, "Complete and Fail need the event to be processing", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "pending", t0)
		appendEvent(t, s, "done", t0)
		appendEvent(t, s, "dead", t0)
		for id, st := range map[string]store.EventStatus{"done": store.EventDone, "dead": store.EventDead} {
			if err := s.SetEventStatus(ctx, id, st, ""); err != nil {
				t.Fatalf("SetEventStatus(%s): %v", id, err)
			}
		}
		for _, id := range []string{"pending", "done", "dead"} {
			isErr(t, s.CompleteEvent(ctx, id, 1), store.ErrConflict, "CompleteEvent("+id+")")
			isErr(t, s.FailEvent(ctx, id, 1, t0, 3, "x"), store.ErrConflict, "FailEvent("+id+")")
		}
		if got := getEvent(t, s, "pending"); got.Status != store.EventPending || got.LastError != "" {
			t.Errorf("rejected FailEvent modified the event: %+v", got)
		}
		isErr(t, s.CompleteEvent(ctx, "missing", 1), store.ErrNotFound, "CompleteEvent(missing)")
		isErr(t, s.FailEvent(ctx, "missing", 1, t0, 3, "x"), store.ErrNotFound, "FailEvent(missing)")
	})

	su.run(t, "of Complete and Fail with one claim's token exactly one succeeds", func(t *testing.T, s store.Store) {
		appendEvent(t, s, "ev", t0)
		if _, err := s.ClaimEvent(t.Context(), t0, lease); err != nil {
			t.Fatal(err)
		}
		var completeErr, failErr error
		parallel(2, func(i int) {
			if i == 0 {
				completeErr = s.CompleteEvent(t.Context(), "ev", 1)
			} else {
				failErr = s.FailEvent(t.Context(), "ev", 1, t0.Add(time.Minute), 3, "x")
			}
		})
		if (completeErr == nil) == (failErr == nil) {
			t.Fatalf("complete = %v, fail = %v; want exactly one to succeed", completeErr, failErr)
		}
		loser := completeErr
		if loser == nil {
			loser = failErr
		}
		isErr(t, loser, store.ErrConflict, "the loser")
	})

	su.run(t, "the queue refuses invalid arguments", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		claim := func(now time.Time, l time.Duration) error { _, err := s.ClaimEvent(ctx, now, l); return err }
		for name, err := range map[string]error{
			"claim zero now":     claim(time.Time{}, lease),
			"claim zero lease":   claim(t0, 0),
			"claim neg lease":    claim(t0, -time.Second),
			"complete empty id":  s.CompleteEvent(ctx, "", 1),
			"complete attempt 0": s.CompleteEvent(ctx, "ev", 0),
			"fail empty id":      s.FailEvent(ctx, "", 1, t0, 3, "x"),
			"fail attempt 0":     s.FailEvent(ctx, "ev", 0, t0, 3, "x"),
			"fail zero retry":    s.FailEvent(ctx, "ev", 1, time.Time{}, 3, "x"),
			"fail zero max":      s.FailEvent(ctx, "ev", 1, t0, 0, "x"),
		} {
			isErr(t, err, store.ErrInvalid, name)
		}
	})
}
