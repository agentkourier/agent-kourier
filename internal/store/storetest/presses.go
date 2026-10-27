package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var roomy = store.PressLimits{Total: 100, PerUser: 100}

func press(id, user string, at time.Time) store.Press {
	return store.Press{InteractionID: id, User: user, Channel: "C1", MessageRef: "1700.01", Inputs: []byte(`{"q0":{"a0":"typed"}}`), PressedAt: at}
}

func samePress(a, b store.Press) bool {
	return a.Seq == b.Seq && a.InteractionID == b.InteractionID && a.User == b.User && a.Channel == b.Channel &&
		a.MessageRef == b.MessageRef && bytes.Equal(a.Inputs, b.Inputs) && a.PressedAt.Equal(b.PressedAt)
}

func pressSeqs(l []store.Press) []int64 {
	var seqs []int64
	for _, p := range l {
		seqs = append(seqs, p.Seq)
	}
	return seqs
}

func (su suite) presses(t *testing.T) {
	// Presses come back in the order they were recorded, whoever pressed, and each press is kept: the
	// store applies no "one press for each person" rule, which is the interaction's (the first complete
	// answer wins).
	su.runReopen(t, "presses are listed in the order they were recorded, all of them, and survive a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		ctx := t.Context()
		var seqs []int64
		who := []string{"U2", "U1", "U2", "U3"}
		for _, w := range who {
			seq, err := s.RecordPress(ctx, press("ix-"+w, w, t0), roomy)
			noErr(t, err, "RecordPress")
			seqs = append(seqs, seq)
		}
		if !slices.IsSorted(seqs) || seqs[0] == seqs[1] || seqs[1] == seqs[2] || seqs[2] == seqs[3] {
			t.Errorf("seqs = %v; want them increasing", seqs)
		}
		check := func(s store.Store) {
			t.Helper()
			got, err := s.ListPresses(ctx)
			if err != nil || len(got) != 4 {
				t.Fatalf("ListPresses = %+v, %v; want 4", got, err)
			}
			for i, w := range who {
				want := press("ix-"+w, w, t0)
				want.Seq = seqs[i]
				if !samePress(got[i], want) {
					t.Errorf("press %d = %+v; want %+v", i, got[i], want)
				}
			}
		}
		check(s)
		check(reopen())
	})

	su.run(t, "a person's repeated presses of one interaction are each recorded", func(t *testing.T, s store.Store) {
		for range 3 {
			_, err := s.RecordPress(t.Context(), press("ix", "U1", t0), roomy)
			noErr(t, err, "RecordPress")
		}
		if got, err := s.ListPresses(t.Context()); err != nil || len(got) != 3 {
			t.Errorf("presses = %+v, %v; want 3", got, err)
		}
	})

	su.run(t, "a press with no inputs is recorded and comes back with none", func(t *testing.T, s store.Store) {
		p := press("ix", "U1", t0)
		p.Inputs = nil
		if _, err := s.RecordPress(t.Context(), p, roomy); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ListPresses(t.Context()); err != nil || len(got) != 1 || len(got[0].Inputs) != 0 {
			t.Errorf("presses = %+v, %v; want one with no inputs", got, err)
		}
	})

	su.run(t, "a press that is incomplete is refused and records nothing", func(t *testing.T, s store.Store) {
		for name, mutate := range map[string]func(*store.Press){
			"no interaction": func(p *store.Press) { p.InteractionID = "" }, "no user": func(p *store.Press) { p.User = "" },
			"no channel": func(p *store.Press) { p.Channel = "" }, "no message": func(p *store.Press) { p.MessageRef = "" },
			"no time": func(p *store.Press) { p.PressedAt = time.Time{} },
		} {
			p := press("ix", "U1", t0)
			mutate(&p)
			_, err := s.RecordPress(t.Context(), p, roomy)
			isErr(t, err, store.ErrInvalid, name)
		}
		for name, lim := range map[string]store.PressLimits{"no limits": {}, "no total": {PerUser: 1}, "no per-user": {Total: 1}} {
			_, err := s.RecordPress(t.Context(), press("ix", "U1", t0), lim)
			isErr(t, err, store.ErrInvalid, name)
		}
		if got, _ := s.ListPresses(t.Context()); len(got) != 0 {
			t.Errorf("%d presses recorded from refused calls; want none", len(got))
		}
	})

	// Past either limit the press is refused, and a processed press frees its place.
	su.run(t, "a press past the limit of the store or of its user is refused, and a processed press frees a place", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		limits := store.PressLimits{Total: 4, PerUser: 2}
		record := func(who string) (int64, error) { return s.RecordPress(ctx, press("ix", who, t0), limits) }
		first, _ := record("U1")
		for _, who := range []string{"U1", "U2", "U3"} {
			if _, err := record(who); err != nil {
				t.Fatalf("%s: %v", who, err)
			}
		}
		_, err := record("U1")
		isErr(t, err, store.ErrLimit, "a third press by one user and a fifth in all")
		_, err = record("U4")
		isErr(t, err, store.ErrLimit, "a fifth press in all")
		noErr(t, s.DeletePress(ctx, first), "DeletePress")
		if _, err := record("U4"); err != nil {
			t.Errorf("after a press was processed: %v; want room", err)
		}
		_, err = record("U3")
		isErr(t, err, store.ErrLimit, "full again")
	})

	su.run(t, "the limit of one user binds with room in the store", func(t *testing.T, s store.Store) {
		limits := store.PressLimits{Total: 10, PerUser: 2}
		for range 2 {
			if _, err := s.RecordPress(t.Context(), press("ix", "U1", t0), limits); err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.RecordPress(t.Context(), press("ix", "U1", t0), limits)
		isErr(t, err, store.ErrLimit, "U1's third press with room in the store")
		if _, err := s.RecordPress(t.Context(), press("ix", "U2", t0), limits); err != nil {
			t.Errorf("U2's first press: %v; want room", err)
		}
	})

	// The limits hold when many presses arrive together: counting and inserting are one step, so exactly
	// the limit succeeds and the rest are refused, for one person and for everyone.
	for name, tc := range map[string]struct {
		limits store.PressLimits
		user   func(i int) string
	}{
		"one user":   {store.PressLimits{Total: 1000, PerUser: 10}, func(int) string { return "U1" }},
		"many users": {store.PressLimits{Total: 10, PerUser: 10}, func(i int) string { return fmt.Sprintf("U%d", i) }},
	} {
		su.run(t, "the press limits hold under concurrency: "+name, func(t *testing.T, s store.Store) {
			var ok, refused atomic.Int32
			parallel(50, func(i int) {
				switch _, err := s.RecordPress(t.Context(), press("ix", tc.user(i), t0), tc.limits); {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, store.ErrLimit):
					refused.Add(1)
				default:
					t.Errorf("RecordPress: %v", err)
				}
			})
			if got, _ := s.ListPresses(t.Context()); ok.Load() != 10 || refused.Load() != 40 || len(got) != 10 {
				t.Errorf("%d recorded, %d refused, %d stored; want exactly 10, 40 and 10", ok.Load(), refused.Load(), len(got))
			}
		})
	}

	// Concurrent presses are all recorded, each with a Seq of its own, and a reader that lists them sees
	// them in Seq order; as with the reply queue, a Seq that is assigned at insert and shown at commit
	// could show a later press before an earlier one.
	su.run(t, "presses recorded at the same time each get their own seq, and are listed in seq order", func(t *testing.T, s store.Store) {
		const n = 32
		parallel(n, func(i int) {
			if _, err := s.RecordPress(t.Context(), press("ix", fmt.Sprint("U", i), t0), roomy); err != nil {
				t.Errorf("RecordPress: %v", err)
			}
		})
		got, err := s.ListPresses(t.Context())
		noErr(t, err, "ListPresses")
		seqs := pressSeqs(got)
		if len(seqs) != n || !slices.IsSorted(seqs) || len(slices.Compact(slices.Clone(seqs))) != n {
			t.Errorf("seqs = %v; want %d distinct, increasing", seqs, n)
		}
	})

	su.run(t, "a processed press is gone, and deleting it again is not an error", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		seq, _ := s.RecordPress(ctx, press("ix", "U1", t0), roomy)
		other, _ := s.RecordPress(ctx, press("ix", "U2", t0), roomy)
		for range 2 {
			noErr(t, s.DeletePress(ctx, seq), "DeletePress")
		}
		noErr(t, s.DeletePress(ctx, 1<<40), "DeletePress of a seq never recorded")
		if got, _ := s.ListPresses(ctx); len(got) != 1 || got[0].Seq != other {
			t.Errorf("presses = %+v; want only the other one", got)
		}
	})

	// A press is purged an hour after its interaction's deadline, and not before; one for an interaction
	// that is not in the store counts from its own time.
	su.run(t, "PurgePresses deletes a press once its interaction's deadline plus the grace has passed", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		i := newInteraction("ix-known", time.Hour) // due at t0 + 1 h
		if _, err := s.CreateInteraction(ctx, i); err != nil {
			t.Fatal(err)
		}
		known, _ := s.RecordPress(ctx, press("ix-known", "U1", t0.Add(30*time.Minute)), roomy)
		stray, _ := s.RecordPress(ctx, press("ix-unknown", "U1", t0.Add(2*time.Hour)), roomy) // pressed at t0 + 2 h
		fresh, _ := s.RecordPress(ctx, press("ix-unknown", "U2", t0.Add(4*time.Hour)), roomy)
		grace := time.Hour
		for _, step := range []struct {
			at   time.Time
			gone int
			left []int64
		}{
			{i.Deadline.Add(grace - time.Microsecond), 0, []int64{known, stray, fresh}}, // a microsecond early
			{i.Deadline.Add(grace), 1, []int64{stray, fresh}},                           // the known press, at its deadline + 1 h
			{t0.Add(3*time.Hour - time.Microsecond), 0, []int64{stray, fresh}},
			{t0.Add(3 * time.Hour), 1, []int64{fresh}}, // the stray, an hour after it was pressed
		} {
			n, err := s.PurgePresses(ctx, step.at, grace)
			got, _ := s.ListPresses(ctx)
			if seqs := pressSeqs(got); err != nil || n != step.gone || !slices.Equal(seqs, step.left) {
				t.Fatalf("purge at %v: %d gone, %v, left %v; want %d gone, left %v", step.at, n, err, seqs, step.gone, step.left)
			}
		}
		_, err := s.PurgePresses(ctx, time.Time{}, grace)
		isErr(t, err, store.ErrInvalid, "a purge with no time")
		_, err = s.PurgePresses(ctx, t0, -time.Second)
		isErr(t, err, store.ErrInvalid, "a purge with a negative grace")
		if n, err := s.PurgePresses(ctx, t0.Add(100*time.Hour), 0); err != nil || n != 1 {
			t.Errorf("a purge with no grace = %d, %v; want the last press, now past its time", n, err)
		}
	})

	// PendingPresses says whether a press of the interaction made at or before a time is waiting.
	su.run(t, "PendingPresses says whether a press made at or before a time is waiting", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		seq, _ := s.RecordPress(ctx, press("ix-1", "U1", t0.Add(time.Minute)), roomy)
		_, _ = s.RecordPress(ctx, press("ix-2", "U2", t0), roomy)
		for _, tc := range []struct {
			id     string
			before time.Time
			want   bool
		}{
			{"ix-1", t0.Add(time.Minute), true},                     // the boundary is inclusive
			{"ix-1", t0.Add(time.Minute - time.Microsecond), false}, // a microsecond before the press
			{"ix-1", t0.Add(time.Hour), true},
			{"ix-2", t0.Add(time.Minute), true},
			{"ix-3", t0.Add(time.Hour), false}, // no press of that interaction
		} {
			if got, err := s.PendingPresses(ctx, tc.id, tc.before); err != nil || got != tc.want {
				t.Errorf("PendingPresses(%s, %v) = %v, %v; want %v", tc.id, tc.before, got, err, tc.want)
			}
		}
		noErr(t, s.DeletePress(ctx, seq), "DeletePress")
		if got, _ := s.PendingPresses(ctx, "ix-1", t0.Add(time.Hour)); got {
			t.Error("a processed press is still pending")
		}
		_, err := s.PendingPresses(ctx, "", t0)
		isErr(t, err, store.ErrInvalid, "no interaction")
		_, err = s.PendingPresses(ctx, "ix-1", time.Time{})
		isErr(t, err, store.ErrInvalid, "no time")
	})

	// Implicit in the interface, which names only presses: an answer typed as a reply in the thread is a
	// press that has not been recorded yet, and the timeout sweep must wait for it too. Today that means a
	// reply that waits in the reply queue and is either pinned to the question, or unpinned, enqueued from
	// the question's creation to the time asked about, in a thread whose question is a pending one that
	// takes text replies and has its prompt posted.
	for _, textReplies := range []bool{false, true} {
		su.run(t, fmt.Sprintf("a reply waiting in the thread of a question delays its timeout only if the question takes text replies (%v)", textReplies), func(t *testing.T, s store.Store) {
			ctx := t.Context()
			i := newInteraction("question", time.Minute)
			i.TextReplies = textReplies
			if _, err := s.CreateInteraction(ctx, i); err != nil {
				t.Fatal(err)
			}
			noErr(t, s.SetInteractionMessageRef(ctx, i.ID, "prompt"), "SetInteractionMessageRef")
			for _, tc := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"before question", t0.Add(-time.Second), false},
				{"after deadline", i.Deadline.Add(time.Microsecond), false},
				{"in time", i.Deadline.Add(-time.Second), textReplies},
			} {
				r := newReply(tc.name, i.Key)
				r.EnqueuedAt = tc.at
				_, err := s.EnqueueReply(ctx, r)
				noErr(t, err, "enqueue")
				pending, err := s.PendingPresses(ctx, i.ID, i.Deadline)
				if err != nil || pending != tc.want {
					t.Fatalf("%s pending=%v error=%v; want %v", tc.name, pending, err, tc.want)
				}
				noErr(t, s.MarkReplyDelivered(ctx, r.ID, t0.Add(time.Hour)), "mark delivered")
			}
			if pending, err := s.PendingPresses(ctx, i.ID, i.Deadline); err != nil || pending {
				t.Fatalf("consumed replies pending=%v error=%v; want none", pending, err)
			}
		})
	}

	su.run(t, "a reply in the thread of a question that is answered, or has no prompt yet, delays nothing", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		noPrompt := newInteraction("no-prompt", time.Minute)
		noPrompt.TextReplies = true
		answered := newInteraction("answered", time.Minute)
		answered.TextReplies = true
		answered.Key = keyOf("2.2")
		for _, i := range []store.Interaction{noPrompt, answered} {
			_, err := s.CreateInteraction(ctx, i)
			noErr(t, err, "create "+i.ID)
		}
		noErr(t, s.SetInteractionMessageRef(ctx, "answered", "prompt"), "SetInteractionMessageRef")
		noErr(t, s.SubmitInteraction(ctx, "answered", []byte("yes"), "U1", t0.Add(time.Second)), "submit")
		for _, i := range []store.Interaction{noPrompt, answered} {
			r := newReply("reply-"+i.ID, i.Key)
			r.EnqueuedAt = t0.Add(time.Second)
			_, err := s.EnqueueReply(ctx, r)
			noErr(t, err, "enqueue")
			if pending, err := s.PendingPresses(ctx, i.ID, i.Deadline); err != nil || pending {
				t.Errorf("%s: pending=%v error=%v; want none", i.ID, pending, err)
			}
		}
	})

	su.run(t, "a reply pinned to a question delays its timeout until it is delivered, in or out of the store", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		r := newReply("r1", key)
		r.EnqueuedAt = t0.Add(time.Minute)
		_, err := s.EnqueueReply(ctx, r)
		noErr(t, err, "enqueue")
		noErr(t, s.ClaimReplyInteraction(ctx, "r1", "q"), "claim")
		for _, tc := range []struct {
			before time.Time
			want   bool
		}{{t0.Add(time.Minute), true}, {t0.Add(time.Minute - time.Microsecond), false}} {
			if got, err := s.PendingPresses(ctx, "q", tc.before); err != nil || got != tc.want {
				t.Errorf("PendingPresses(q, %v) = %v, %v; want %v", tc.before, got, err, tc.want)
			}
		}
		noErr(t, s.MarkReplyDelivered(ctx, "r1", t0.Add(time.Hour)), "mark delivered")
		if got, _ := s.PendingPresses(ctx, "q", t0.Add(time.Hour)); got {
			t.Error("a delivered reply is still pending")
		}
	})
}
