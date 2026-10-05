package storetest

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func addOutbox(t *testing.T, s store.Store, id string, created time.Time, payload string) {
	t.Helper()
	o := store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxReply, Channel: "C1", CreatedAt: created, Payload: payload}
	if _, err := s.CreateOutbox(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func getOutbox(t *testing.T, s store.Store, id string) store.OutboxEntry {
	t.Helper()
	o, err := s.GetOutbox(t.Context(), id)
	if err != nil {
		t.Fatalf("GetOutbox(%s): %v", id, err)
	}
	return o
}

func unposted(t *testing.T, s store.Store) []store.OutboxEntry {
	t.Helper()
	l, err := s.ListUnposted(t.Context())
	if err != nil {
		t.Fatalf("ListUnposted: %v", err)
	}
	return l
}

func outboxIDs(l []store.OutboxEntry) []string {
	var ids []string
	for _, o := range l {
		ids = append(ids, o.ID)
	}
	return ids
}

// sameOutbox compares what an outbox row is, times as instants, leaving out its trace context.
func sameOutbox(a, b store.OutboxEntry) bool {
	return a.ID == b.ID && a.EventID == b.EventID && a.Role == b.Role && a.Channel == b.Channel && a.Thread == b.Thread &&
		a.CreatedAt.Equal(b.CreatedAt) && a.MessageRef == b.MessageRef && a.Payload == b.Payload && a.AbandonedAt.Equal(b.AbandonedAt)
}

func (su suite) outbox(t *testing.T) {
	su.run(t, "an outbox row is stored once by its ID and starts unposted", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if l := unposted(t, s); len(l) != 0 {
			t.Fatalf("empty outbox lists %d rows", len(l))
		}
		_, err := s.GetOutbox(ctx, "nope")
		isErr(t, err, store.ErrNotFound, "GetOutbox of a missing row")

		base := store.OutboxEntry{ID: "x", EventID: "e", Role: store.OutboxNotice, Channel: "C", CreatedAt: t0}
		for name, mutate := range map[string]func(*store.OutboxEntry){
			"no ID":      func(o *store.OutboxEntry) { o.ID = "" },
			"no EventID": func(o *store.OutboxEntry) { o.EventID = "" },
			"bad role":   func(o *store.OutboxEntry) { o.Role = "bogus" },
			"no channel": func(o *store.OutboxEntry) { o.Channel = "" },
			"no time":    func(o *store.OutboxEntry) { o.CreatedAt = time.Time{} },
		} {
			bad := base
			mutate(&bad)
			_, err := s.CreateOutbox(ctx, bad)
			isErr(t, err, store.ErrInvalid, name)
		}

		o := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", Thread: "1.1", CreatedAt: t0, Payload: `{"text":"hi"}`}
		if c, err := s.CreateOutbox(ctx, o); err != nil || !c {
			t.Fatalf("CreateOutbox = %v, %v", c, err)
		}
		dup := o
		dup.Payload = `{"text":"changed"}`
		if c, err := s.CreateOutbox(ctx, dup); err != nil || c {
			t.Fatalf("duplicate CreateOutbox = %v, %v; want false, nil", c, err)
		}
		if got := getOutbox(t, s, o.ID); !sameOutbox(got, o) {
			t.Fatalf("GetOutbox = %+v; want the first row, %+v", got, o)
		}
		// The other rows of one source event are rows of their own: the ID is the key, not the EventID.
		root := store.OutboxEntry{ID: "ev1/root", EventID: "ev1", Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0.Add(time.Second)}
		if c, err := s.CreateOutbox(ctx, root); err != nil || !c {
			t.Fatalf("a second row of the same event = %v, %v; want it created", c, err)
		}
	})

	su.run(t, "a new row is unposted and not abandoned whatever the caller says", func(t *testing.T, s store.Store) {
		o := store.OutboxEntry{ID: "o", EventID: "e", Role: store.OutboxReply, Channel: "C", CreatedAt: t0, Payload: "{}", MessageRef: "9.9", AbandonedAt: t0.Add(time.Hour)}
		if _, err := s.CreateOutbox(t.Context(), o); err != nil {
			t.Fatal(err)
		}
		got := getOutbox(t, s, "o")
		if got.MessageRef != "" || !got.AbandonedAt.IsZero() || got.Payload != "{}" {
			t.Errorf("row = %+v; want it unposted and not abandoned, with its payload", got)
		}
		if l := unposted(t, s); len(l) != 1 {
			t.Errorf("unposted = %+v; want the row", l)
		}
	})

	su.run(t, "every role is accepted", func(t *testing.T, s store.Store) {
		for _, role := range []store.OutboxRole{store.OutboxRoot, store.OutboxReply, store.OutboxDigest, store.OutboxNotice, store.OutboxReaction} {
			e := store.OutboxEntry{ID: "e/" + string(role), EventID: "e", Role: role, Channel: "C", CreatedAt: t0}
			if created, err := s.CreateOutbox(t.Context(), e); err != nil || !created {
				t.Errorf("CreateOutbox with role %q = %v, %v; want it created", role, created, err)
			}
			if got := getOutbox(t, s, e.ID); got.Role != role {
				t.Errorf("role = %q; want %q", got.Role, role)
			}
		}
	})

	// Rows made at the same time may come in any order: the outbox reconciles each row on its own.
	su.run(t, "ListUnposted lists the unposted rows oldest first", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		a := store.OutboxEntry{ID: "ev1/root", EventID: "ev1", Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0}
		b := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", Thread: "1.1", CreatedAt: t0.Add(time.Second)}
		for _, o := range []store.OutboxEntry{b, a} {
			if c, err := s.CreateOutbox(ctx, o); err != nil || !c {
				t.Fatalf("create %s = %v, %v", o.ID, c, err)
			}
		}
		if l := unposted(t, s); len(l) != 2 || !sameOutbox(l[0], a) || !sameOutbox(l[1], b) {
			t.Fatalf("unposted = %+v; want a then b", l)
		}
		noErr(t, s.RecordMessageRef(ctx, a.ID, "1700.01"), "RecordMessageRef")
		if l := unposted(t, s); len(l) != 1 || !sameOutbox(l[0], b) {
			t.Fatalf("unposted after record = %+v; want only b", l)
		}

		ties := []string{"tie/b", "tie/B", "tie/a", "tie/9"}
		for _, id := range ties {
			addOutbox(t, s, id, t0.Add(time.Hour), "")
		}
		addOutbox(t, s, "last", t0.Add(2*time.Hour), "")
		if got := outboxIDs(unposted(t, s)); !inTieGroups(got, []string{b.ID}, ties, []string{"last"}) {
			t.Errorf("unposted = %v; want ev1/reply, then the four made at one time in any order, then last", got)
		}
	})

	// This is the adoption of a post: recovery finds the message that a crashed send did make, and records it.
	su.run(t, "recording a message ref adopts the post once, clears its payload, and never repoints it", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		o := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", Thread: "1.1", CreatedAt: t0, Payload: `{"text":"hi"}`}
		_, err := s.CreateOutbox(ctx, o)
		noErr(t, err, "CreateOutbox")
		for range 2 { // the second record is the same-ref no-op, and still leaves it empty
			noErr(t, s.RecordMessageRef(ctx, o.ID, "1700.01"), "RecordMessageRef")
			want := o
			want.Payload, want.MessageRef = "", "1700.01"
			if got := getOutbox(t, s, o.ID); !sameOutbox(got, want) {
				t.Fatalf("GetOutbox after record = %+v; want %+v: the text lives in the chat now, not in the database too", got, want)
			}
		}
		isErr(t, s.RecordMessageRef(ctx, o.ID, "1700.99"), store.ErrConflict, "a different ref")
		isErr(t, s.RecordMessageRef(ctx, "nope", "1"), store.ErrNotFound, "RecordMessageRef of a missing row")
		isErr(t, s.RecordMessageRef(ctx, "", "1"), store.ErrInvalid, "RecordMessageRef with no ID")
		isErr(t, s.RecordMessageRef(ctx, o.ID, ""), store.ErrInvalid, "RecordMessageRef with no ref")
		if got := getOutbox(t, s, o.ID); got.MessageRef != "1700.01" {
			t.Errorf("ref = %q after the refused calls; want it unchanged", got.MessageRef)
		}
		if _, err := s.CreateOutbox(ctx, o); err != nil {
			t.Fatal(err)
		}
		if got := getOutbox(t, s, o.ID); got.MessageRef != "1700.01" || got.Payload != "" {
			t.Errorf("a repeat of CreateOutbox changed the posted row: %+v", got)
		}
	})

	su.run(t, "of many adoptions of one row with different refs exactly one wins", func(t *testing.T, s store.Store) {
		addOutbox(t, s, "o", t0, `{"text":"hi"}`)
		const n = 12
		errs := make([]error, n)
		parallel(n, func(i int) { errs[i] = s.RecordMessageRef(t.Context(), "o", fmt.Sprintf("1700.%02d", i)) })
		winner := ""
		for i, err := range errs {
			if err != nil {
				isErr(t, err, store.ErrConflict, fmt.Sprintf("adoption %d", i))
				continue
			}
			if winner != "" {
				t.Errorf("two refs were recorded: %s and 1700.%02d", winner, i)
			}
			winner = fmt.Sprintf("1700.%02d", i)
		}
		if got := getOutbox(t, s, "o"); winner == "" || got.MessageRef != winner {
			t.Errorf("row ref = %q; want the winner, %q", got.MessageRef, winner)
		}
	})

	su.run(t, "many creates of one row create it exactly once", func(t *testing.T, s store.Store) {
		const n = 16
		created := make([]bool, n)
		parallel(n, func(i int) {
			o := store.OutboxEntry{ID: "same", EventID: "e", Role: store.OutboxRoot, Channel: "C", CreatedAt: t0, Payload: fmt.Sprint(i)}
			var err error
			if created[i], err = s.CreateOutbox(t.Context(), o); err != nil {
				t.Errorf("create %d: %v", i, err)
			}
		})
		wins := 0
		for _, c := range created {
			if c {
				wins++
			}
		}
		if wins != 1 || len(unposted(t, s)) != 1 {
			t.Errorf("%d creates reported the insert and %d rows are stored; want 1 and 1", wins, len(unposted(t, s)))
		}
	})

	su.run(t, "a payload is opaque text, kept until the message is posted", func(t *testing.T, s store.Store) {
		for id, payload := range map[string]string{
			"json":    `{"text":"hi \"there\" \\ \n","emoji":"🚨 é ü 日本語"}`,
			"quotes":  `'; DROP TABLE outbox; -- %_ \`,
			"spaces":  "  leading and trailing  ",
			"long":    strings.Repeat(`{"k":"é"}`, 100_000),
			"nothing": "",
		} {
			addOutbox(t, s, id, t0, payload)
			if got := getOutbox(t, s, id); got.Payload != payload {
				t.Errorf("%s: payload = %d bytes; want the %d bytes written", id, len(got.Payload), len(payload))
			}
		}
	})

	su.run(t, "AbandonOutbox gives up an unposted row once, keeps the first time and clears the payload", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		addOutbox(t, s, "a", t0, `{"text":"hi"}`)
		addOutbox(t, s, "posted", t0, `{"text":"hi"}`)
		noErr(t, s.RecordMessageRef(ctx, "posted", "5.5"), "RecordMessageRef")
		at := t0.Add(time.Hour)
		if ok, err := s.AbandonOutbox(ctx, "a", at); err != nil || !ok {
			t.Fatalf("AbandonOutbox = %v, %v; want true", ok, err)
		}
		if got := getOutbox(t, s, "a"); !got.AbandonedAt.Equal(at) || got.Payload != "" || got.MessageRef != "" {
			t.Fatalf("abandoned row = %+v; want the time kept, the payload cleared, no ref", got)
		}
		if l := unposted(t, s); len(l) != 0 {
			t.Fatalf("unposted = %v; an abandoned row is not unposted work", outboxIDs(l))
		}
		if ok, err := s.AbandonOutbox(ctx, "a", at.Add(time.Hour)); err != nil || ok { // the second is not news, and keeps the first time
			t.Fatalf("second AbandonOutbox = %v, %v; want false, nil", ok, err)
		}
		if got := getOutbox(t, s, "a"); !got.AbandonedAt.Equal(at) {
			t.Fatalf("abandoned at %v after a second call; want %v", got.AbandonedAt, at)
		}
		_, err := s.AbandonOutbox(ctx, "nope", at)
		isErr(t, err, store.ErrNotFound, "a missing row")
		_, err = s.AbandonOutbox(ctx, "posted", at)
		isErr(t, err, store.ErrConflict, "a posted row")
	})

	// A stored 0 means "not abandoned" in a store that keeps the time as a number, so a time at or before
	// the epoch would clear the payload and leave the row as unposted work with nothing to post.
	su.run(t, "a time of abandoning must be after the Unix epoch", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		addOutbox(t, s, "a", t0.Add(-8*24*time.Hour), `{"text":"hi"}`)
		for _, bad := range []time.Time{{}, time.Unix(0, 0), time.Unix(-5, 0)} {
			_, err := s.AbandonOutbox(ctx, "a", bad)
			isErr(t, err, store.ErrInvalid, fmt.Sprintf("AbandonOutbox at %v", bad))
			_, err = s.AbandonOutboxBefore(ctx, t0, bad)
			isErr(t, err, store.ErrInvalid, fmt.Sprintf("AbandonOutboxBefore as of %v", bad))
			_, err = s.AbandonOutboxBefore(ctx, bad, t0)
			isErr(t, err, store.ErrInvalid, fmt.Sprintf("AbandonOutboxBefore before %v", bad))
		}
		if got := getOutbox(t, s, "a"); got.Payload == "" || !got.AbandonedAt.IsZero() {
			t.Fatalf("row after the refusals = %+v; want it untouched", got)
		}
	})

	// By design: a late lookup that finds the message must be able to adopt it.
	su.run(t, "a message ref can be recorded on an abandoned row", func(t *testing.T, s store.Store) {
		addOutbox(t, s, "a", t0, `{"text":"hi"}`)
		_, err := s.AbandonOutbox(t.Context(), "a", t0)
		noErr(t, err, "AbandonOutbox")
		noErr(t, s.RecordMessageRef(t.Context(), "a", "5.5"), "RecordMessageRef on an abandoned row")
		if got := getOutbox(t, s, "a"); got.MessageRef != "5.5" || got.AbandonedAt.IsZero() {
			t.Fatalf("row = %+v; want the ref recorded and the abandon kept", got)
		}
	})

	// The retention purge takes only a row that is old, unrecorded, not yet abandoned and has a payload.
	su.run(t, "AbandonOutboxBefore takes only old, unposted rows that still hold a payload", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		cut := t0.Add(-7 * 24 * time.Hour)
		addOutbox(t, s, "old", cut.Add(-time.Second), `{"text":"hi"}`)
		addOutbox(t, s, "boundary", cut, `{"text":"hi"}`)              // not older than the cut
		addOutbox(t, s, "recent", t0.Add(-time.Hour), `{"text":"hi"}`) // young
		addOutbox(t, s, "stream", cut.Add(-time.Hour), "")             // no payload to purge
		addOutbox(t, s, "done", cut.Add(-time.Hour), `{"text":"hi"}`)  // recorded: its payload is gone
		addOutbox(t, s, "gone", cut.Add(-time.Hour), `{"text":"hi"}`)  // abandoned before
		noErr(t, s.RecordMessageRef(ctx, "done", "5.5"), "RecordMessageRef")
		early := t0.Add(-time.Hour)
		_, err := s.AbandonOutbox(ctx, "gone", early)
		noErr(t, err, "AbandonOutbox")
		ids, err := s.AbandonOutboxBefore(ctx, cut, t0)
		if err != nil || !slices.Equal(ids, []string{"old"}) {
			t.Fatalf("AbandonOutboxBefore = %v, %v; want only old", ids, err)
		}
		for id, want := range map[string]time.Time{"old": t0, "gone": early, "boundary": {}, "recent": {}, "stream": {}, "done": {}} {
			if got := getOutbox(t, s, id); !got.AbandonedAt.Equal(want) {
				t.Errorf("%s abandoned at %v; want %v", id, got.AbandonedAt, want)
			}
		}
		if got := getOutbox(t, s, "old"); got.Payload != "" {
			t.Errorf("the purged row kept its payload %q", got.Payload)
		}
		if ids, err := s.AbandonOutboxBefore(ctx, cut, t0); err != nil || len(ids) != 0 {
			t.Fatalf("second purge = %v, %v; want nothing", ids, err)
		}
	})

	su.run(t, "of concurrent retention purges each row is abandoned by one of them", func(t *testing.T, s store.Store) {
		const rows, purgers = 40, 6
		cut := t0.Add(time.Hour)
		for i := range rows {
			addOutbox(t, s, fmt.Sprintf("o%02d", i), t0, `{"text":"hi"}`)
		}
		var mu sync.Mutex
		got := map[string]int{}
		parallel(purgers, func(int) {
			ids, err := s.AbandonOutboxBefore(t.Context(), cut, cut)
			if err != nil {
				t.Errorf("AbandonOutboxBefore: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				got[id]++
			}
		})
		if len(got) != rows {
			t.Errorf("%d rows were abandoned; want %d", len(got), rows)
		}
		for id, n := range got {
			if n != 1 {
				t.Errorf("row %s was abandoned by %d purges; want 1", id, n)
			}
		}
	})
}
