package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// newInteraction returns a valid interaction created at t0 that is due after d.
func newInteraction(id string, d time.Duration) store.Interaction {
	return store.Interaction{
		ID: id, Key: key, TaskID: "task1", Kind: store.InteractionAskUser,
		Request: []byte(`{"q":1}`), CreatedAt: t0, Deadline: t0.Add(d),
	}
}

func createInteraction(t *testing.T, s store.Store, id string) {
	t.Helper()
	if _, err := s.CreateInteraction(t.Context(), newInteraction(id, time.Hour)); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func getInteraction(t *testing.T, s store.Store, id string) store.Interaction {
	t.Helper()
	i, err := s.GetInteraction(t.Context(), id)
	if err != nil {
		t.Fatalf("GetInteraction(%s): %v", id, err)
	}
	return i
}

// sameInteraction compares what an interaction is, times as instants, and a nil slice as an empty one.
func sameInteraction(a, b store.Interaction) bool {
	return a.ID == b.ID && a.Key == b.Key && a.TaskID == b.TaskID && a.Kind == b.Kind && a.State == b.State &&
		bytes.Equal(a.Request, b.Request) && bytes.Equal(a.Answer, b.Answer) && a.MessageRef == b.MessageRef &&
		a.CreatedAt.Equal(b.CreatedAt) && a.Deadline.Equal(b.Deadline) && a.DecidedAt.Equal(b.DecidedAt) &&
		a.DeliveredAt.Equal(b.DeliveredAt) && a.Decider == b.Decider && a.TextReplies == b.TextReplies
}

func interactionIDs(l []store.Interaction) []string {
	var ids []string
	for _, i := range l {
		ids = append(ids, i.ID)
	}
	return ids
}

func (su suite) interactions(t *testing.T) {
	su.run(t, "an interaction is stored pending, once by its ID, and read back whole", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		i := newInteraction("i1", time.Hour)
		i.TextReplies = true
		i.Request = []byte{0, 1, 2, 255, 254} // opaque bytes
		i.MessageRef = "ignored"
		i.State, i.Decider = store.InteractionAnswered, "U1" // ignored on create, like MessageRef
		i.Answer, i.DecidedAt, i.DeliveredAt = []byte("x"), t0, t0
		if c, err := s.CreateInteraction(ctx, i); err != nil || !c {
			t.Fatalf("create = %v, %v; want true, nil", c, err)
		}
		dup := newInteraction("i1", 2*time.Hour)
		if c, err := s.CreateInteraction(ctx, dup); err != nil || c {
			t.Fatalf("duplicate create = %v, %v; want false, nil", c, err)
		}
		want := i
		want.State, want.Decider, want.MessageRef, want.Answer, want.DecidedAt, want.DeliveredAt = store.InteractionPending, "", "", nil, time.Time{}, time.Time{}
		if got := getInteraction(t, s, "i1"); !sameInteraction(got, want) {
			t.Fatalf("get = %+v; want %+v", got, want)
		}
		_, err := s.GetInteraction(ctx, "nope")
		isErr(t, err, store.ErrNotFound, "get of a missing interaction")

		for name, mutate := range map[string]func(*store.Interaction){
			"empty ID":        func(i *store.Interaction) { i.ID = "" },
			"empty key part":  func(i *store.Interaction) { i.Key.Thread = "" },
			"empty task ID":   func(i *store.Interaction) { i.TaskID = "" },
			"unknown kind":    func(i *store.Interaction) { i.Kind = "bogus" },
			"zero created":    func(i *store.Interaction) { i.CreatedAt = time.Time{} },
			"zero deadline":   func(i *store.Interaction) { i.Deadline = time.Time{} },
			"deadline = made": func(i *store.Interaction) { i.Deadline = i.CreatedAt },
		} {
			bad := newInteraction("bad", time.Hour)
			mutate(&bad)
			_, err := s.CreateInteraction(ctx, bad)
			isErr(t, err, store.ErrInvalid, name+": create")
		}
	})

	su.run(t, "an interaction with no request comes back with an empty one", func(t *testing.T, s store.Store) {
		i := newInteraction("i1", time.Hour)
		i.Request = nil
		if _, err := s.CreateInteraction(t.Context(), i); err != nil {
			t.Fatal(err)
		}
		if got := getInteraction(t, s, "i1"); len(got.Request) != 0 {
			t.Errorf("request = %q; want empty", got.Request)
		}
	})

	su.run(t, "every kind of interaction is accepted", func(t *testing.T, s store.Store) {
		for _, k := range []store.InteractionKind{store.InteractionAskUser, store.InteractionToolApproval, store.InteractionNested} {
			i := newInteraction("i-"+string(k), time.Hour)
			i.Kind = k
			if c, err := s.CreateInteraction(t.Context(), i); err != nil || !c {
				t.Errorf("create of kind %q = %v, %v", k, c, err)
			}
			if got := getInteraction(t, s, i.ID); got.Kind != k {
				t.Errorf("kind = %q; want %q", got.Kind, k)
			}
		}
	})

	su.run(t, "many creates of one interaction create it exactly once", func(t *testing.T, s store.Store) {
		const n = 16
		created := make([]bool, n)
		parallel(n, func(i int) {
			var err error
			if created[i], err = s.CreateInteraction(t.Context(), newInteraction("same", time.Hour)); err != nil {
				t.Errorf("create %d: %v", i, err)
			}
		})
		wins := 0
		for _, c := range created {
			if c {
				wins++
			}
		}
		if wins != 1 {
			t.Errorf("%d creates reported the insert; want exactly 1", wins)
		}
	})

	su.run(t, "ListDueInteractions lists pending interactions at or past their deadline, earliest deadline first", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for _, i := range []store.Interaction{newInteraction("late", 3*time.Hour), newInteraction("due", time.Hour), newInteraction("soon", 2*time.Hour)} {
			if _, err := s.CreateInteraction(ctx, i); err != nil {
				t.Fatalf("create %s: %v", i.ID, err)
			}
		}
		ids := func(now time.Time) []string {
			t.Helper()
			l, err := s.ListDueInteractions(ctx, now)
			noErr(t, err, "ListDueInteractions")
			return interactionIDs(l)
		}
		// A deadline equal to now is due; one microsecond later is not.
		for _, c := range []struct {
			now  time.Time
			want []string
		}{
			{t0.Add(time.Hour - 1), nil},
			{t0.Add(time.Hour), []string{"due"}},
			{t0.Add(2 * time.Hour), []string{"due", "soon"}},
			{t0.Add(4 * time.Hour), []string{"due", "soon", "late"}},
		} {
			if got := ids(c.now); !slices.Equal(got, c.want) {
				t.Errorf("due at %v = %v; want %v", c.now.Sub(t0), got, c.want)
			}
		}
		// Decided rows are never due.
		noErr(t, s.SubmitInteraction(ctx, "due", []byte("yes"), "U1", t0.Add(time.Minute)), "answer")
		noErr(t, s.ResolveInteraction(ctx, "soon", store.InteractionCancelled, t0.Add(time.Minute)), "cancel")
		if got := ids(t0.Add(4 * time.Hour)); !slices.Equal(got, []string{"late"}) {
			t.Errorf("due after answering and cancelling = %v; want late", got)
		}
		_, err := s.ListDueInteractions(ctx, time.Time{})
		isErr(t, err, store.ErrInvalid, "ListDueInteractions with a zero now")
	})

	// Interactions due at the same time may come in any order: the sweep takes a batch from the front and
	// times each out on its own.
	su.run(t, "interactions due at the same time are all listed, after earlier ones and before later ones", func(t *testing.T, s store.Store) {
		ties := []string{"b", "B", "a", "9"}
		for _, id := range ties {
			createInteraction(t, s, id)
		}
		for id, d := range map[string]time.Duration{"early": 30 * time.Minute, "late": 2 * time.Hour} {
			_, err := s.CreateInteraction(t.Context(), newInteraction(id, d))
			noErr(t, err, "create "+id)
		}
		l, err := s.ListDueInteractions(t.Context(), t0.Add(3*time.Hour))
		noErr(t, err, "ListDueInteractions")
		if got := interactionIDs(l); !inTieGroups(got, []string{"early"}, ties, []string{"late"}) {
			t.Errorf("due = %v; want early, then the four due at one time in any order, then late", got)
		}
	})

	su.run(t, "the first complete answer wins and names who gave it", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		now := t0.Add(time.Minute)
		createInteraction(t, s, "i1")
		noErr(t, s.SubmitInteraction(ctx, "i1", []byte("yes"), "U1", now), "first submit")
		got := getInteraction(t, s, "i1")
		if got.State != store.InteractionAnswered || string(got.Answer) != "yes" || got.Decider != "U1" || !got.DecidedAt.Equal(now) {
			t.Fatalf("after submit = %+v", got)
		}
		err := s.SubmitInteraction(ctx, "i1", []byte("no"), "U2", now)
		isErr(t, err, store.ErrConflict, "second submit")
		if err != nil && !strings.Contains(err.Error(), "U1") {
			t.Errorf("second submit = %q; want it to name the decider, U1", err)
		}
		if got = getInteraction(t, s, "i1"); string(got.Answer) != "yes" || got.Decider != "U1" {
			t.Fatalf("second submit changed the answer: %+v", got)
		}

		// At the deadline, or after the sweep timed it out, a submit is expired and changes nothing.
		createInteraction(t, s, "i2")
		createInteraction(t, s, "i3")
		noErr(t, s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour)), "resolve")
		for _, c := range []struct {
			id        string
			wantState store.InteractionState
		}{{"i2", store.InteractionPending}, {"i3", store.InteractionTimedOut}} {
			isErr(t, s.SubmitInteraction(ctx, c.id, []byte("late"), "U1", t0.Add(time.Hour)), store.ErrExpired, c.id+": submit at the deadline")
			if got = getInteraction(t, s, c.id); got.State != c.wantState || got.Decider != "" {
				t.Errorf("%s: expired submit changed the row: %+v", c.id, got)
			}
		}
		isErr(t, s.SubmitInteraction(ctx, "nope", []byte("x"), "U1", now), store.ErrNotFound, "submit of a missing interaction")
		isErr(t, s.SubmitInteraction(ctx, "", []byte("x"), "U1", now), store.ErrInvalid, "submit with no ID")
		isErr(t, s.SubmitInteraction(ctx, "i2", nil, "U1", now), store.ErrInvalid, "submit with no answer")
		isErr(t, s.SubmitInteraction(ctx, "i2", []byte("x"), "", now), store.ErrInvalid, "submit with no decider")
		isErr(t, s.SubmitInteraction(ctx, "i2", []byte("x"), "U1", time.Time{}), store.ErrInvalid, "submit with no time")
	})

	su.run(t, "of many answers to one interaction exactly one is recorded", func(t *testing.T, s store.Store) {
		createInteraction(t, s, "i1")
		const n = 16
		errs := make([]error, n)
		parallel(n, func(i int) {
			errs[i] = s.SubmitInteraction(t.Context(), "i1", []byte("a"), fmt.Sprintf("U%d", i), t0.Add(time.Minute))
		})
		winners := 0
		for i, err := range errs {
			if err == nil {
				winners++
			} else if !isConflict(err) {
				t.Errorf("submit %d = %v; want nil or ErrConflict", i, err)
			}
		}
		got := getInteraction(t, s, "i1")
		if winners != 1 || got.Decider == "" {
			t.Fatalf("winners = %d, row = %+v; want exactly one", winners, got)
		}
		for _, e := range errs {
			if e != nil && !strings.Contains(e.Error(), got.Decider) {
				t.Errorf("conflict %q does not name the winner %s", e, got.Decider)
			}
		}
	})

	su.run(t, "an answer and a cancel at the same time: exactly one of them is applied", func(t *testing.T, s store.Store) {
		const rounds = 10
		for r := range rounds {
			id := fmt.Sprint("i", r)
			createInteraction(t, s, id)
			var submitErr, cancelErr error
			parallel(2, func(i int) {
				if i == 0 {
					submitErr = s.SubmitInteraction(t.Context(), id, []byte("a"), "U1", t0.Add(time.Minute))
				} else {
					cancelErr = s.ResolveInteraction(t.Context(), id, store.InteractionCancelled, t0.Add(time.Minute))
				}
			})
			got := getInteraction(t, s, id)
			switch got.State {
			case store.InteractionAnswered:
				if submitErr != nil || !isConflict(cancelErr) {
					t.Errorf("%s answered: submit = %v, cancel = %v; want nil and ErrConflict", id, submitErr, cancelErr)
				}
			case store.InteractionCancelled:
				if cancelErr != nil || !isConflict(submitErr) {
					t.Errorf("%s cancelled: submit = %v, cancel = %v; want ErrConflict and nil", id, submitErr, cancelErr)
				}
			default:
				t.Errorf("%s is %s; want answered or cancelled", id, got.State)
			}
		}
	})

	su.run(t, "an interaction is resolved only from pending, and a timeout only once it is due", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		now := t0.Add(time.Minute)
		for _, id := range []string{"i1", "i2", "i3"} {
			createInteraction(t, s, id)
		}
		noErr(t, s.ResolveInteraction(ctx, "i1", store.InteractionCancelled, now), "resolve pending")
		if got := getInteraction(t, s, "i1"); got.State != store.InteractionCancelled || !got.DecidedAt.Equal(now) || got.Decider != "" {
			t.Fatalf("after resolve = %+v", got)
		}
		noErr(t, s.SubmitInteraction(ctx, "i2", []byte("yes"), "U1", now), "submit")
		// A timeout before the deadline is refused, so the answer window stays open; at the deadline it works.
		isErr(t, s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour-1)), store.ErrConflict, "early timeout")
		if got := getInteraction(t, s, "i3"); got.State != store.InteractionPending {
			t.Errorf("early timeout moved the row to %s", got.State)
		}
		if err := s.ResolveInteraction(ctx, "i3", store.InteractionTimedOut, t0.Add(time.Hour)); err != nil {
			t.Errorf("timeout at the deadline: %v", err)
		}
		for _, id := range []string{"i1", "i2"} { // cancelled and answered rows must not move
			isErr(t, s.ResolveInteraction(ctx, id, store.InteractionTimedOut, now), store.ErrConflict, "resolve "+id)
		}
		if got := getInteraction(t, s, "i2"); got.State != store.InteractionAnswered {
			t.Errorf("resolve moved an answered row to %s", got.State)
		}
		// A cancelled interaction refuses a later answer.
		isErr(t, s.SubmitInteraction(ctx, "i1", []byte("x"), "U1", now), store.ErrConflict, "submit to a cancelled interaction")
		isErr(t, s.ResolveInteraction(ctx, "nope", store.InteractionTimedOut, now), store.ErrNotFound, "resolve of a missing interaction")
		for _, st := range []store.InteractionState{store.InteractionPending, store.InteractionAnswered, "bogus"} {
			isErr(t, s.ResolveInteraction(ctx, "i2", st, now), store.ErrInvalid, fmt.Sprintf("resolve to %q", st))
		}
		isErr(t, s.ResolveInteraction(ctx, "i2", store.InteractionTimedOut, time.Time{}), store.ErrInvalid, "resolve with a zero now")
		isErr(t, s.ResolveInteraction(ctx, "", store.InteractionTimedOut, now), store.ErrInvalid, "resolve with no ID")
	})

	su.run(t, "the message ref is set once, in any state", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		createInteraction(t, s, "i1")
		createInteraction(t, s, "i2")
		set := func(id, ref string) error { return s.SetInteractionMessageRef(ctx, id, ref) }
		noErr(t, set("i1", "1700.01"), "first set")
		if err := set("i1", "1700.01"); err != nil {
			t.Errorf("same ref again = %v; want no-op", err)
		}
		isErr(t, set("i1", "1700.99"), store.ErrConflict, "a different ref")
		isErr(t, set("nope", "1"), store.ErrNotFound, "a missing interaction")
		isErr(t, set("", "1"), store.ErrInvalid, "an empty id")
		isErr(t, set("i1", ""), store.ErrInvalid, "an empty ref")
		// An answer can land before the ref is recorded; the ref is still accepted.
		noErr(t, s.SubmitInteraction(ctx, "i2", []byte("yes"), "U1", t0.Add(time.Minute)), "submit")
		noErr(t, set("i2", "1700.02"), "set after answered")
		if got := getInteraction(t, s, "i2"); got.MessageRef != "1700.02" || got.State != store.InteractionAnswered {
			t.Fatalf("after set = %+v", got)
		}
	})

	su.run(t, "of many message refs set at once exactly one is kept", func(t *testing.T, s store.Store) {
		createInteraction(t, s, "i1")
		const n = 12
		errs := make([]error, n)
		parallel(n, func(i int) { errs[i] = s.SetInteractionMessageRef(t.Context(), "i1", fmt.Sprintf("1700.%02d", i)) })
		winner := ""
		for i, err := range errs {
			if err == nil {
				if winner != "" {
					t.Errorf("two refs were set: %s and 1700.%02d", winner, i)
				}
				winner = fmt.Sprintf("1700.%02d", i)
			} else {
				isErr(t, err, store.ErrConflict, fmt.Sprintf("set %d", i))
			}
		}
		if got := getInteraction(t, s, "i1"); winner == "" || got.MessageRef != winner {
			t.Errorf("ref = %q; want the winner, %q", got.MessageRef, winner)
		}
	})

	// MarkInteractionDelivered records, once, that an answer reached the session: the first mark stands, a
	// repeat is no error, and only an answered interaction has an answer to deliver.
	su.run(t, "the delivery of an answer is marked once, and only for an answered interaction", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		createInteraction(t, s, "i1")
		isErr(t, s.MarkInteractionDelivered(ctx, "i1", t0.Add(time.Minute)), store.ErrConflict, "mark of a pending interaction: there is no answer yet")
		noErr(t, s.SubmitInteraction(ctx, "i1", []byte(`{"a":1}`), "U1", t0.Add(time.Minute)), "submit")
		if got := getInteraction(t, s, "i1"); !got.DeliveredAt.IsZero() {
			t.Fatalf("an answer just recorded is delivered at %v; want it not known to be", got.DeliveredAt)
		}
		for _, at := range []time.Time{t0.Add(2 * time.Minute), t0.Add(3 * time.Minute)} { // the second is a repeat
			if err := s.MarkInteractionDelivered(ctx, "i1", at); err != nil {
				t.Fatalf("mark at %v = %v", at, err)
			}
		}
		got := getInteraction(t, s, "i1")
		if !got.DeliveredAt.Equal(t0.Add(2 * time.Minute)) {
			t.Errorf("delivered at %v; want the first mark, %v", got.DeliveredAt, t0.Add(2*time.Minute))
		}
		if listed, err := s.ListDecidedInteractions(ctx, t0); err != nil || len(listed) != 1 || !listed[0].DeliveredAt.Equal(got.DeliveredAt) {
			t.Errorf("listed %+v, %v; want the delivery time with the row", listed, err)
		}
		createInteraction(t, s, "i2")
		noErr(t, s.ResolveInteraction(ctx, "i2", store.InteractionCancelled, t0.Add(time.Minute)), "cancel")
		isErr(t, s.MarkInteractionDelivered(ctx, "i2", t0.Add(time.Minute)), store.ErrConflict, "mark of a cancelled interaction")
		isErr(t, s.MarkInteractionDelivered(ctx, "nope", t0), store.ErrNotFound, "mark of a missing interaction")
		isErr(t, s.MarkInteractionDelivered(ctx, "", t0), store.ErrInvalid, "mark with no ID")
		isErr(t, s.MarkInteractionDelivered(ctx, "i1", time.Time{}), store.ErrInvalid, "mark with no time")
	})

	// ListDecidedInteractions returns the interactions decided since a time, in the order they were decided:
	// those a prompt may still show controls for. A pending one is not decided.
	su.run(t, "ListDecidedInteractions lists what was decided since a time, in the order it was decided", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
		for _, id := range []string{"pending", "old", "just-before", "at-since", "answered", "cancelled"} {
			if _, err := s.CreateInteraction(ctx, newInteraction(id, 10*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.CreateInteraction(ctx, newInteraction("timed", time.Hour)); err != nil {
			t.Fatal(err)
		}
		noErr(t, s.SubmitInteraction(ctx, "old", []byte(`{}`), "U1", at(1)), "submit old")
		noErr(t, s.SubmitInteraction(ctx, "answered", []byte(`{}`), "U1", at(5)), "submit answered")
		noErr(t, s.ResolveInteraction(ctx, "timed", store.InteractionTimedOut, at(3)), "time out")
		noErr(t, s.ResolveInteraction(ctx, "cancelled", store.InteractionCancelled, at(4)), "cancel")
		// The boundary: one decided exactly at since is listed, and one a microsecond before it is not.
		noErr(t, s.ResolveInteraction(ctx, "at-since", store.InteractionCancelled, at(2)), "cancel at since")
		noErr(t, s.ResolveInteraction(ctx, "just-before", store.InteractionCancelled, at(2).Add(-time.Microsecond)), "cancel before since")

		got, err := s.ListDecidedInteractions(ctx, at(2))
		noErr(t, err, "ListDecidedInteractions")
		if want := []string{"at-since", "timed", "cancelled", "answered"}; !slices.Equal(interactionIDs(got), want) {
			t.Fatalf("ListDecidedInteractions = %v; want %v: decided since (the boundary included), by when, and not pending or older", interactionIDs(got), want)
		}
		if got[3].State != store.InteractionAnswered || got[3].Decider != "U1" || string(got[3].Answer) != "{}" || got[1].State != store.InteractionTimedOut {
			t.Errorf("rows = %+v; want them whole", got)
		}
		_, err = s.ListDecidedInteractions(ctx, time.Time{})
		isErr(t, err, store.ErrInvalid, "a zero time")
	})

	// Decisions at the same time may come in any order: the repair of prompts treats each interaction on its own.
	su.run(t, "interactions decided at the same time are all listed, after earlier ones and before later ones", func(t *testing.T, s store.Store) {
		ties := []string{"b", "B", "a", "9"}
		for _, id := range append([]string{"early", "late"}, ties...) {
			createInteraction(t, s, id)
		}
		for id, at := range map[string]time.Duration{"early": 30 * time.Second, "late": 2 * time.Minute} {
			noErr(t, s.ResolveInteraction(t.Context(), id, store.InteractionCancelled, t0.Add(at)), "cancel "+id)
		}
		for _, id := range ties {
			noErr(t, s.ResolveInteraction(t.Context(), id, store.InteractionCancelled, t0.Add(time.Minute)), "cancel "+id)
		}
		l, err := s.ListDecidedInteractions(t.Context(), t0)
		noErr(t, err, "ListDecidedInteractions")
		if got := interactionIDs(l); !inTieGroups(got, []string{"early"}, ties, []string{"late"}) {
			t.Errorf("decided = %v; want early, then the four decided at one time in any order, then late", got)
		}
	})

	su.run(t, "PendingTextQuestion finds the pending question of the thread that takes text replies, and no other", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.PendingTextQuestion(ctx, key)
		isErr(t, err, store.ErrNotFound, "no interaction at all")
		isErr(t, errOf(s.PendingTextQuestion(ctx, store.ConversationKey{Connection: "slack", Channel: "C1"})), store.ErrInvalid, "an incomplete key")

		create := func(id string, k store.ConversationKey, text bool, after time.Duration) {
			t.Helper()
			i := newInteraction(id, time.Hour)
			i.Key, i.TextReplies, i.CreatedAt = k, text, t0.Add(after)
			i.Deadline = i.CreatedAt.Add(time.Hour)
			if _, err := s.CreateInteraction(ctx, i); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}
		create("button", key, false, 0) // pending, but it takes no text reply
		create("elsewhere", keyOf("2.2"), true, 0)
		create("answered", key, true, 0)
		create("timed-out", key, true, 0)
		create("cancelled", key, true, 0)
		noErr(t, s.SubmitInteraction(ctx, "answered", []byte("yes"), "U1", t0.Add(time.Minute)), "answer")
		noErr(t, s.ResolveInteraction(ctx, "timed-out", store.InteractionTimedOut, t0.Add(2*time.Hour)), "time out")
		noErr(t, s.ResolveInteraction(ctx, "cancelled", store.InteractionCancelled, t0.Add(time.Minute)), "cancel")
		_, err = s.PendingTextQuestion(ctx, key)
		isErr(t, err, store.ErrNotFound, "only a non-text, a settled and another thread's question")

		create("text", key, true, time.Minute)
		got, err := s.PendingTextQuestion(ctx, key)
		if err != nil || got.ID != "text" || !got.TextReplies || got.State != store.InteractionPending || got.Key != key || got.TaskID != "task1" {
			t.Fatalf("PendingTextQuestion = %+v, %v; want the pending text question text, whole", got, err)
		}
		if other, err := s.PendingTextQuestion(ctx, keyOf("2.2")); err != nil || other.ID != "elsewhere" {
			t.Errorf("PendingTextQuestion of the other thread = %+v, %v; want elsewhere", other, err)
		}

		// Implicit: with two pending (a session has one task, so it should not happen) the newest is the one asked.
		create("newer", key, true, 2*time.Minute)
		if got, err := s.PendingTextQuestion(ctx, key); err != nil || got.ID != "newer" {
			t.Errorf("PendingTextQuestion with two pending = %+v, %v; want the newest, newer", got, err)
		}
		noErr(t, s.ResolveInteraction(ctx, "newer", store.InteractionCancelled, t0.Add(3*time.Minute)), "cancel newer")
		if got, err := s.PendingTextQuestion(ctx, key); err != nil || got.ID != "text" {
			t.Errorf("PendingTextQuestion after newer closed = %+v, %v; want text", got, err)
		}
	})
}

// errOf drops the first of two results, for a call whose value the test does not look at.
func errOf(_ store.Interaction, err error) error { return err }

func isConflict(err error) bool { return errors.Is(err, store.ErrConflict) }
