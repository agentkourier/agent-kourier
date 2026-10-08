package storetest

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func newReply(id string, k store.ConversationKey) store.Reply {
	return store.Reply{ID: id, Key: k, UserID: "U1", MessageRef: "1700.01", Text: "hello", EnqueuedAt: t0}
}

// replyClaimer is the part of a store that the interactions package asks for by type assertion and that
// store.ReplyQueue does not list: pinning a queued answer to one question.
type replyClaimer interface {
	ClaimReplyInteraction(ctx context.Context, id, interaction string) error
}

func (su suite) replyQueue(t *testing.T) {
	su.run(t, "HasReply sees queued and delivered replies", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		has := func(id string) bool {
			t.Helper()
			ok, err := s.HasReply(ctx, id)
			noErr(t, err, fmt.Sprintf("HasReply(%q)", id))
			return ok
		}
		if has("r1") {
			t.Fatal("HasReply of an ID never enqueued is true")
		}
		_, err := s.EnqueueReply(ctx, newReply("r1", key))
		noErr(t, err, "enqueue")
		if !has("r1") || has("r2") {
			t.Fatal("HasReply after enqueueing r1: want r1 yes, r2 no")
		}
		noErr(t, s.MarkReplyDelivered(ctx, "r1", t0), "mark delivered")
		if !has("r1") {
			t.Error("HasReply of a delivered reply is false; want the tombstone to answer yes")
		}
		_, err = s.HasReply(ctx, "")
		isErr(t, err, store.ErrInvalid, "HasReply with no ID")
	})

	su.run(t, "HasReplies returns the set of the IDs that were enqueued, queued or delivered", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		found := func(ids ...string) map[string]bool {
			t.Helper()
			got, err := s.HasReplies(ctx, ids)
			noErr(t, err, fmt.Sprintf("HasReplies(%q)", ids))
			if got == nil {
				t.Fatalf("HasReplies(%q) returned a nil map; want an empty one when nothing was found", ids)
			}
			return got
		}
		sameSet := func(what string, got map[string]bool, want ...string) {
			t.Helper()
			if len(got) != len(want) {
				t.Errorf("%s = %v; want %v", what, got, want)
			}
			for _, id := range want {
				if !got[id] {
					t.Errorf("%s = %v; want %v", what, got, want)
				}
			}
		}
		sameSet("no IDs", found())
		sameSet("an empty slice", found([]string{}...))
		sameSet("IDs never enqueued", found("r1", "r2"))

		other := keyOf("2.2")
		for id, k := range map[string]store.ConversationKey{"r1": key, "r2": key, "x1": other} {
			_, err := s.EnqueueReply(ctx, newReply(id, k))
			noErr(t, err, "enqueue "+id)
		}
		noErr(t, s.MarkReplyDelivered(ctx, "r2", t0), "mark delivered")
		sameSet("queued, delivered, missing and another session's, in any order", found("zz", "x1", "r2", "nope", "r1"), "r1", "r2", "x1")
		sameSet("a repeated ID", found("r1", "r1", "zz", "r1", "zz"), "r1")
		sameSet("only missing IDs, repeated", found("zz", "zz"))
		for _, id := range []string{"r1", "r2", "x1"} { // the batch and the single lookup tell the same
			if one, err := s.HasReply(ctx, id); err != nil || !one || !found(id)[id] {
				t.Errorf("HasReply(%s) = %v, %v; want the batch to agree", id, one, err)
			}
		}

		_, err := s.HasReplies(ctx, []string{"r1", "", "r2"})
		isErr(t, err, store.ErrInvalid, "HasReplies with an empty ID among others")
		ids := make([]string, store.MaxReplyIDs+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("m%d", i)
		}
		_, err = s.HasReplies(ctx, ids)
		isErr(t, err, store.ErrInvalid, "HasReplies with more than MaxReplyIDs IDs")
	})

	su.run(t, "HasReplies finds every ID of a list longer than one statement can carry", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		const total = 2500 // above SQLite's old limit of 999 variables and several times any chunk the adapters use
		// The enqueued IDs sit on both sides of every multiple of 500 and 1000, so that a list cut into chunks of
		// any size from 100 to 1000 puts some at the start and the end of a chunk.
		at := []int{0, 1, 99, 100, 101, 499, 500, 501, 998, 999, 1000, 1001, 1499, 1500, 1999, 2000, 2499}
		ids := make([]string, total)
		for i := range ids {
			ids[i] = fmt.Sprintf("bulk-%d", i)
		}
		want := map[string]bool{}
		for _, i := range at {
			_, err := s.EnqueueReply(ctx, newReply(ids[i], key))
			noErr(t, err, "enqueue "+ids[i])
			want[ids[i]] = true
		}
		got, err := s.HasReplies(ctx, append(ids, ids[:50]...)) // 50 of them twice
		noErr(t, err, "HasReplies")
		if len(got) != len(want) {
			t.Fatalf("HasReplies found %d of %d IDs", len(got), len(want))
		}
		for id := range want {
			if !got[id] {
				t.Errorf("HasReplies missed %s", id)
			}
		}
	})

	su.run(t, "replies of a session come back first in, first out, and sessions do not touch one another", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		other := keyOf("2.2")
		_, err := s.NextReply(ctx, key)
		isErr(t, err, store.ErrNotFound, "next on an empty queue")
		// Enqueue times run backwards on purpose: order must follow the store's sequence, not the clock.
		for i, r := range []store.Reply{newReply("r1", key), newReply("x1", other), newReply("r2", key), newReply("r3", key)} {
			r.EnqueuedAt = t0.Add(-time.Duration(i) * time.Hour)
			if ok, err := s.EnqueueReply(ctx, r); err != nil || !ok {
				t.Fatalf("enqueue %s = %v, %v", r.ID, ok, err)
			}
		}
		if ok, err := s.EnqueueReply(ctx, newReply("r2", key)); err != nil || ok {
			t.Fatalf("duplicate enqueue = %v, %v; want false, nil", ok, err)
		}
		var last int64
		for _, want := range []string{"r1", "r2", "r3"} {
			got, err := s.NextReply(ctx, key)
			if err != nil || got.ID != want || got.UserID != "U1" || got.Text != "hello" || got.MessageRef != "1700.01" || got.Key != key || got.Sequence <= last {
				t.Fatalf("next = %+v, %v; want %s with a sequence above %d", got, err, want, last)
			}
			last = got.Sequence
			if again, _ := s.NextReply(ctx, key); again.ID != want {
				t.Fatalf("NextReply does not leave the head in place: %s", again.ID)
			}
			noErr(t, s.MarkReplyDelivered(ctx, want, t0), "mark "+want+" delivered")
		}
		_, err = s.NextReply(ctx, key)
		isErr(t, err, store.ErrNotFound, "next after draining")
		if got, err := s.NextReply(ctx, other); err != nil || got.ID != "x1" {
			t.Fatalf("other session's queue = %+v, %v; want x1 untouched", got, err)
		}
	})

	// A delivered reply stays as a tombstone: its ID is not queued again, whatever its content.
	su.run(t, "a delivered reply is a tombstone: marking again is a no-op and the ID is not queued twice", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.EnqueueReply(ctx, newReply("r1", key))
		noErr(t, err, "enqueue")
		noErr(t, s.MarkReplyDelivered(ctx, "r1", t0), "mark")
		noErr(t, s.MarkReplyDelivered(ctx, "r1", t0.Add(time.Hour)), "mark twice")
		if ok, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil || ok {
			t.Fatalf("re-enqueue of a delivered ID = %v, %v; want false, nil", ok, err)
		}
		_, err = s.NextReply(ctx, key)
		isErr(t, err, store.ErrNotFound, "NextReply after the reply was delivered")
		_, err = s.NextReplyAfter(ctx, key, 0)
		isErr(t, err, store.ErrNotFound, "NextReplyAfter after the reply was delivered")
		isErr(t, s.MarkReplyDelivered(ctx, "nope", t0), store.ErrNotFound, "mark of an unknown ID")
		isErr(t, s.MarkReplyDelivered(ctx, "", t0), store.ErrInvalid, "mark with no ID")
		isErr(t, s.MarkReplyDelivered(ctx, "r1", time.Time{}), store.ErrInvalid, "mark with no time")
	})

	// An ID is the turn's messageId, derived from the chat message: it is unique in the whole queue,
	// not only in a session.
	su.run(t, "an ID is queued once for the whole store, not once for each session", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if ok, err := s.EnqueueReply(ctx, newReply("r1", key)); err != nil || !ok {
			t.Fatalf("first enqueue = %v, %v", ok, err)
		}
		if ok, err := s.EnqueueReply(ctx, newReply("r1", keyOf("2.2"))); err != nil || ok {
			t.Fatalf("the ID again for another session = %v, %v; want false, nil", ok, err)
		}
		_, err := s.NextReply(ctx, keyOf("2.2"))
		isErr(t, err, store.ErrNotFound, "the other session's queue")
	})

	su.run(t, "a reply needs an ID, a key, a user, a message and a time, and not text", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for name, mutate := range map[string]func(*store.Reply){
			"empty ID":       func(r *store.Reply) { r.ID = "" },
			"empty key part": func(r *store.Reply) { r.Key.Thread = "" },
			"empty user":     func(r *store.Reply) { r.UserID = "" },
			"empty ts":       func(r *store.Reply) { r.MessageRef = "" },
			"zero time":      func(r *store.Reply) { r.EnqueuedAt = time.Time{} },
		} {
			bad := newReply("bad", key)
			mutate(&bad)
			_, err := s.EnqueueReply(ctx, bad)
			isErr(t, err, store.ErrInvalid, name+": enqueue")
		}
		_, err := s.NextReply(ctx, store.ConversationKey{})
		isErr(t, err, store.ErrInvalid, "next with an empty key")
		_, err = s.NextReplyAfter(ctx, key, -1)
		isErr(t, err, store.ErrInvalid, "NextReplyAfter with a negative cursor")
		if has, _ := s.HasReply(ctx, "bad"); has {
			t.Error("a refused reply was queued")
		}

		empty := newReply("empty", key)
		empty.Text = ""
		if ok, err := s.EnqueueReply(ctx, empty); err != nil || !ok {
			t.Fatalf("enqueue with no text = %v, %v; want it queued", ok, err)
		}
		if got, err := s.NextReply(ctx, key); err != nil || got.ID != "empty" || got.Text != "" {
			t.Errorf("next = %+v, %v; want the reply with no text, still waiting", got, err)
		}
	})

	su.run(t, "NextReplyAfter skips replies up to a cursor and leaves them queued", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for _, id := range []string{"earlier", "answer", "later"} {
			_, err := s.EnqueueReply(ctx, newReply(id, key))
			noErr(t, err, "enqueue "+id)
		}
		head, err := s.NextReply(ctx, key)
		noErr(t, err, "NextReply")
		answer, err := s.NextReplyAfter(ctx, key, head.Sequence)
		if err != nil || answer.ID != "answer" {
			t.Fatalf("cursor = %+v, %v", answer, err)
		}
		noErr(t, s.MarkReplyDelivered(ctx, answer.ID, t0), "mark")
		if got, err := s.NextReply(ctx, key); err != nil || got.ID != "earlier" {
			t.Fatalf("earlier turn lost: %+v, %v", got, err)
		}
		if got, err := s.NextReplyAfter(ctx, key, answer.Sequence); err != nil || got.ID != "later" {
			t.Fatalf("later reply = %+v, %v", got, err)
		}
		_, err = s.NextReplyAfter(ctx, key, 1<<62)
		isErr(t, err, store.ErrNotFound, "NextReplyAfter past the last reply")
	})

	// The context window of a mentioned turn starts at the previous turn's message (spec 8.2, D38), so the previous row
	// is the session's own, whatever state it is in.
	su.run(t, "PreviousReply is the session's row just below a sequence, delivered rows and pinned ones included", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		other := keyOf("2.2")
		rows := []store.Reply{
			{ID: "a1", Key: key, MessageRef: "1700.10"},
			{ID: "x1", Key: other, MessageRef: "1700.11"}, // another session's, between two of this one's
			{ID: "a2", Key: key, MessageRef: "1700.20"},
			{ID: "a3", Key: key, MessageRef: "1700.30"},
			{ID: "a4", Key: key, MessageRef: "1700.40"},
			{ID: "x2", Key: other, MessageRef: "1700.41"},
		}
		for _, r := range rows {
			r.UserID, r.Text, r.EnqueuedAt = "U1", "hello", t0
			if ok, err := s.EnqueueReply(ctx, r); err != nil || !ok {
				t.Fatalf("enqueue %s = %v, %v", r.ID, ok, err)
			}
		}
		// Nothing is delivered yet, so walking each queue from the start gives every row's sequence.
		seq := map[string]int64{}
		for _, k := range []store.ConversationKey{key, other} {
			var cursor int64
			for {
				r, err := s.NextReplyAfter(ctx, k, cursor)
				if isNotFound(err) {
					break
				}
				noErr(t, err, "walk")
				seq[r.ID], cursor = r.Sequence, r.Sequence
			}
		}
		if len(seq) != len(rows) {
			t.Fatalf("walked %v; want the %d rows", seq, len(rows))
		}
		// a2 is delivered (a tombstone, its text cleared) and a3 is pinned to a question: both are still rows.
		noErr(t, s.MarkReplyDelivered(ctx, "a2", t0), "deliver a2")
		c, ok := s.(replyClaimer)
		if !ok {
			t.Fatal("the store has no ClaimReplyInteraction")
		}
		noErr(t, c.ClaimReplyInteraction(ctx, "a3", "q1"), "pin a3")

		for _, tc := range []struct{ below, want string }{
			{"a4", "a3"}, // pinned: still the previous row
			{"a3", "a2"}, // delivered: still the previous row
			{"a2", "a1"}, // x1 sits between them and is another session's
		} {
			got, err := s.PreviousReply(ctx, key, seq[tc.below])
			if err != nil || got.ID != tc.want || got.Key != key {
				t.Errorf("PreviousReply below %s = %+v, %v; want %s", tc.below, got, err, tc.want)
			}
		}
		got, err := s.PreviousReply(ctx, key, seq["a2"])
		if err != nil || got.MessageRef != "1700.10" || got.UserID != "U1" || got.Sequence != seq["a1"] {
			t.Errorf("PreviousReply = %+v, %v; want the whole row of a1 with its message ref, user and sequence", got, err)
		}
		if got, err := s.PreviousReply(ctx, key, seq["a3"]); err != nil || got.Text != "" {
			t.Errorf("PreviousReply below a3 = %+v, %v; want the tombstone of a2, whose text is cleared", got, err)
		}
		// A sequence that is none of the session's still finds the greatest row below it, whoever's the sequence was.
		if got, err := s.PreviousReply(ctx, key, seq["x2"]); err != nil || got.ID != "a4" {
			t.Errorf("PreviousReply below x2's sequence = %+v, %v; want a4, the last row of this session below it", got, err)
		}
		if got, err := s.PreviousReply(ctx, other, seq["x2"]); err != nil || got.ID != "x1" {
			t.Errorf("PreviousReply of the other session = %+v, %v; want x1", got, err)
		}

		_, err = s.PreviousReply(ctx, key, seq["a1"])
		isErr(t, err, store.ErrNotFound, "PreviousReply of the first row, with only another session's below it")
		_, err = s.PreviousReply(ctx, keyOf("9.9"), seq["x2"])
		isErr(t, err, store.ErrNotFound, "PreviousReply of a session with no rows")
		_, err = s.PreviousReply(ctx, store.ConversationKey{}, seq["a2"])
		isErr(t, err, store.ErrInvalid, "PreviousReply with an empty key")
		_, err = s.PreviousReply(ctx, key, 0)
		isErr(t, err, store.ErrInvalid, "PreviousReply below sequence 0")
	})

	su.run(t, "many enqueues of one ID queue it exactly once", func(t *testing.T, s store.Store) {
		const n = 16
		queued := make([]bool, n)
		parallel(n, func(i int) {
			var err error
			if queued[i], err = s.EnqueueReply(t.Context(), newReply("same", key)); err != nil {
				t.Errorf("enqueue %d: %v", i, err)
			}
		})
		wins := 0
		for _, ok := range queued {
			if ok {
				wins++
			}
		}
		if wins != 1 {
			t.Errorf("%d enqueues reported queueing; want exactly 1", wins)
		}
		noErr(t, s.MarkReplyDelivered(t.Context(), "same", t0), "mark")
		_, err := s.NextReply(t.Context(), key)
		isErr(t, err, store.ErrNotFound, "next after the one reply was delivered")
	})

	su.run(t, "replies enqueued at the same time to one session come out in one order, each once", func(t *testing.T, s store.Store) {
		const n = 32
		parallel(n, func(i int) {
			if _, err := s.EnqueueReply(t.Context(), newReply(fmt.Sprintf("r%02d", i), key)); err != nil {
				t.Errorf("enqueue %d: %v", i, err)
			}
		})
		var last int64
		seen := map[string]bool{}
		for range n {
			got, err := s.NextReply(t.Context(), key)
			if err != nil || got.Sequence <= last {
				t.Fatalf("next = %+v, %v; want a sequence above %d", got, err, last)
			}
			last = got.Sequence
			seen[got.ID] = true
			noErr(t, s.MarkReplyDelivered(t.Context(), got.ID, t0), "mark delivered")
		}
		if len(seen) != n {
			t.Errorf("%d distinct replies came out; want %d", len(seen), n)
		}
	})

	// An implementation whose sequence is assigned when a row is inserted, and whose row becomes
	// visible when its transaction commits, can show a reader the reply with sequence 6 before the one
	// with sequence 5. A reader that walks the queue with a cursor then never sees 5. The queue must
	// make a reply visible only after every reply with a lower sequence in its session is.
	su.run(t, "a reader that walks a session's queue with a cursor never misses a reply enqueued at the same time", func(t *testing.T, s store.Store) {
		const writers, perWriter = 8, 20
		var finished atomic.Bool
		type seenReply struct {
			id  string
			seq int64
		}
		var seen []seenReply
		pollerDone := make(chan struct{})
		go func() {
			defer close(pollerDone)
			var cursor int64
			for {
				done := finished.Load() // read before the query: every enqueue is committed if it is true
				r, err := s.NextReplyAfter(t.Context(), key, cursor)
				switch {
				case err == nil:
					if r.Sequence <= cursor { // a store that does not move the cursor on would loop for ever
						t.Errorf("NextReplyAfter(%d) returned sequence %d", cursor, r.Sequence)
						return
					}
					seen = append(seen, seenReply{r.ID, r.Sequence})
					cursor = r.Sequence
				case !isNotFound(err):
					t.Errorf("NextReplyAfter: %v", err)
					return
				case done:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
		parallel(writers, func(w int) {
			for i := range perWriter {
				if _, err := s.EnqueueReply(t.Context(), newReply(fmt.Sprintf("w%d-%02d", w, i), key)); err != nil {
					t.Errorf("enqueue: %v", err)
				}
			}
		})
		finished.Store(true)
		<-pollerDone
		if want := writers * perWriter; len(seen) != want {
			ids := map[string]bool{}
			for _, r := range seen {
				ids[r.id] = true
			}
			t.Errorf("the cursor reader saw %d of %d replies: a reply that committed after a later one was skipped", len(ids), want)
		}
	})

	su.run(t, "sessions queued to at the same time are each in order", func(t *testing.T, s store.Store) {
		const sessions, perSession = 4, 12
		parallel(sessions*perSession, func(i int) {
			k := keyOf(fmt.Sprint("s", i%sessions))
			if _, err := s.EnqueueReply(t.Context(), newReply(fmt.Sprintf("r%02d", i), k)); err != nil {
				t.Errorf("enqueue %d: %v", i, err)
			}
		})
		for sn := range sessions {
			k := keyOf(fmt.Sprint("s", sn))
			var seqs []int64
			for range perSession + 1 { // one more than there are, so a reply that stays queued after its mark is seen
				got, err := s.NextReply(t.Context(), k)
				if isNotFound(err) {
					break
				}
				noErr(t, err, "next")
				seqs = append(seqs, got.Sequence)
				noErr(t, s.MarkReplyDelivered(t.Context(), got.ID, t0), "mark")
			}
			if len(seqs) != perSession || !slices.IsSorted(seqs) {
				t.Errorf("session %d came out as %v; want %d replies in increasing sequence", sn, seqs, perSession)
			}
		}
	})

	// ClaimReplyInteraction is not in store.ReplyQueue, but the interactions package asks the store for it
	// by type assertion, and a store without it cannot answer a text question.
	su.run(t, "a queued answer is pinned to one question and cannot answer another", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		c, ok := s.(replyClaimer)
		if !ok {
			t.Fatal("the store has no ClaimReplyInteraction(ctx, id, interaction) error: text replies need it")
		}
		for _, id := range []string{"r1", "r2"} {
			_, err := s.EnqueueReply(ctx, newReply(id, key))
			noErr(t, err, "enqueue "+id)
		}
		if got, _ := s.NextReply(ctx, key); got.InteractionID != "" {
			t.Fatalf("a reply is queued pinned to %q", got.InteractionID)
		}
		noErr(t, c.ClaimReplyInteraction(ctx, "r2", "q1"), "claim")
		noErr(t, c.ClaimReplyInteraction(ctx, "r2", "q1"), "the same claim again")
		// Implicit: today a claim for another question fails with ErrNotFound, the error of an update
		// that matched no row, and not with ErrConflict. Only the failure is pinned.
		if err := c.ClaimReplyInteraction(ctx, "r2", "q2"); err == nil {
			t.Error("a claim of a reply already pinned to another question succeeded")
		}
		isErr(t, c.ClaimReplyInteraction(ctx, "nope", "q1"), store.ErrNotFound, "claim of an unknown reply")
		isErr(t, c.ClaimReplyInteraction(ctx, "", "q1"), store.ErrInvalid, "claim with no reply")
		isErr(t, c.ClaimReplyInteraction(ctx, "r1", ""), store.ErrInvalid, "claim with no question")

		head, err := s.NextReply(ctx, key)
		if err != nil || head.ID != "r1" || head.InteractionID != "" {
			t.Fatalf("head = %+v, %v; want r1 unpinned: a claim does not reorder the queue", head, err)
		}
		pinned, err := s.NextReplyAfter(ctx, key, head.Sequence)
		if err != nil || pinned.ID != "r2" || pinned.InteractionID != "q1" {
			t.Fatalf("after the head = %+v, %v; want r2 pinned to q1", pinned, err)
		}
	})

	su.run(t, "a reply enqueued pinned to a question stays pinned, and a duplicate does not change it", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		plain, pinned := newReply("plain", key), newReply("pinned", key)
		pinned.InteractionID = "q1"
		for _, r := range []store.Reply{plain, pinned} {
			if ok, err := s.EnqueueReply(ctx, r); err != nil || !ok {
				t.Fatalf("enqueue %s = %v, %v", r.ID, ok, err)
			}
		}
		again := pinned
		again.InteractionID = "q2"
		if ok, err := s.EnqueueReply(ctx, again); err != nil || ok {
			t.Fatalf("duplicate enqueue = %v, %v; want false, nil", ok, err)
		}
		head, err := s.NextReply(ctx, key)
		if err != nil || head.ID != "plain" || head.InteractionID != "" {
			t.Fatalf("head = %+v, %v; want plain, unpinned", head, err)
		}
		got, err := s.NextReplyAfter(ctx, key, head.Sequence)
		if err != nil || got.ID != "pinned" || got.InteractionID != "q1" {
			t.Fatalf("after the head = %+v, %v; want pinned, still pinned to q1", got, err)
		}
	})

	// Replies are queued by key and need no session row: intake enqueues before the session exists, and
	// none of the tests above makes one.
	su.run(t, "a long reply is stored whole", func(t *testing.T, s store.Store) {
		long := newReply("long", key)
		long.Text = strings.Repeat("é", 100_000)
		if _, err := s.EnqueueReply(t.Context(), long); err != nil {
			t.Fatal(err)
		}
		if got, err := s.NextReply(t.Context(), key); err != nil || got.Text != long.Text {
			t.Errorf("next = %d bytes of text, %v; want the text of %d bytes", len(got.Text), err, len(long.Text))
		}
	})
}
