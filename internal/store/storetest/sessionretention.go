package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A session is deleted a fixed time after it expires, with the rows that belong to it (data protection, DP-3). The
// caller owns the horizon; the store owns what is never deleted, and so the suite holds both stores to the same list.

// house is a thread with a row of each kind that belongs to a session: two delivered replies, a settled interaction with a
// press, and four outbox rows: a reply posted to the thread, an abandoned notice, and the queued reaction of its first
// reply with the removal of it. The reaction rows are made by the outbox itself (outbox.EnqueueReaction and
// EnqueueUnreaction), from the source the session manager gives the notice ("<reply ID>/queued", session.queuedID): that
// is the shape production writes, with no thread, and the only link the sweep has to the session is the source.
// Its rows are named for the thread.
type house struct {
	key     store.ConversationKey
	binding string
	last    time.Time
}

// built is what one house holds, which is what deleting it should count.
var built = store.PurgedSessions{Sessions: 1, Replies: 2, Interactions: 1, Presses: 1, Outbox: 4}

func (h house) id(what string) string { return h.key.Thread + "-" + what }

// queued is the source of the queued reaction of the reply what: what the session manager names the notice.
func (h house) queued(what string) string { return h.id(what) + "/queued" }

// reaction asks the outbox, as the sink does, for the queued reaction of the reply what, and returns its row's ID.
func (h house) reaction(t *testing.T, s store.Store, what string) string {
	t.Helper()
	id, err := outbox.New(outbox.Config{Store: s, Now: func() time.Time { return h.last }}).
		EnqueueReaction(t.Context(), outbox.Reaction{Source: h.queued(what), Channel: h.key.Channel, TS: "7." + h.key.Thread, Name: "hourglass_flowing_sand"})
	noErr(t, err, "reaction of "+what)
	return id
}

func (h house) build(t *testing.T, s store.Store) {
	t.Helper()
	ctx := t.Context()
	noErr(t, s.UpsertSession(ctx, store.Session{Key: h.key, ContextID: "ctx", Binding: h.binding, LastActivity: h.last}), "session "+h.key.Thread)
	for _, n := range []string{"r1", "r2"} {
		r := newReply(h.id(n), h.key)
		r.EnqueuedAt = h.last
		if ok, err := s.EnqueueReply(ctx, r); err != nil || !ok {
			t.Fatalf("enqueue %s = %v, %v", r.ID, ok, err)
		}
		noErr(t, s.MarkReplyDelivered(ctx, r.ID, h.last), "deliver "+r.ID)
	}
	i := newInteraction(h.id("ask"), time.Hour)
	i.Key = h.key
	_, err := s.CreateInteraction(ctx, i)
	noErr(t, err, "interaction "+i.ID)
	noErr(t, s.ResolveInteraction(ctx, i.ID, store.InteractionTimedOut, i.Deadline), "settle "+i.ID)
	_, err = s.RecordPress(ctx, press(i.ID, "U1", t0), roomy)
	noErr(t, err, "press on "+i.ID)
	for _, o := range []store.OutboxEntry{
		{ID: h.id("post"), EventID: h.id("r1"), Role: store.OutboxReply, Channel: h.key.Channel, Thread: h.key.Thread},
		{ID: h.id("note"), EventID: "mention-" + h.key.Thread, Role: store.OutboxNotice, Channel: h.key.Channel, Thread: h.key.Thread},
	} {
		o.CreatedAt, o.Payload = h.last, `{"text":"hi"}`
		_, err := s.CreateOutbox(ctx, o)
		noErr(t, err, "outbox "+o.ID)
	}
	noErr(t, s.RecordMessageRef(ctx, h.id("post"), "9."+h.key.Thread), "post")
	// The reaction was made, and then taken off when the turn was sent: the two rows of one source.
	react := h.reaction(t, s, "r1")
	noErr(t, s.RecordMessageRef(ctx, react, "7."+h.key.Thread), "react")
	unreact, err := outbox.New(outbox.Config{Store: s, Now: func() time.Time { return h.last }}).
		EnqueueUnreaction(ctx, outbox.Reaction{Source: h.queued("r1"), Channel: h.key.Channel, TS: "7." + h.key.Thread, Name: "hourglass_flowing_sand"})
	noErr(t, err, "unreaction")
	noErr(t, s.RecordMessageRef(ctx, unreact, "7."+h.key.Thread), "unreact")
	_, err = s.AbandonOutbox(ctx, h.id("note"), h.last.Add(time.Minute))
	noErr(t, err, "abandon "+h.id("note"))
}

// present counts the rows of the house that are in the store: every kind when it is whole, none when it is gone.
func (h house) present(t *testing.T, s store.Store) (sessions, replies, interactions, presses, rows int) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.GetSession(ctx, h.key, 1000*ttl, h.last); err == nil {
		sessions++
	} else if !isNotFound(err) {
		t.Fatalf("GetSession(%s): %v", h.key.Thread, err)
	}
	for _, n := range []string{"r1", "r2"} {
		if ok, err := s.HasReply(ctx, h.id(n)); err != nil {
			t.Fatalf("HasReply(%s): %v", h.id(n), err)
		} else if ok {
			replies++
		}
	}
	if _, err := s.GetInteraction(ctx, h.id("ask")); err == nil {
		interactions++
	} else if !isNotFound(err) {
		t.Fatalf("GetInteraction(%s): %v", h.id("ask"), err)
	}
	l, err := s.ListPresses(ctx)
	noErr(t, err, "ListPresses")
	for _, p := range l {
		if p.InteractionID == h.id("ask") {
			presses++
		}
	}
	for _, id := range []string{h.id("post"), h.id("note"), outbox.ID(h.queued("r1"), store.OutboxReaction), outbox.UnreactionID(h.queued("r1"))} {
		if _, err := s.GetOutbox(ctx, id); err == nil {
			rows++
		} else if !isNotFound(err) {
			t.Fatalf("GetOutbox(%s): %v", id, err)
		}
	}
	return
}

// whole fails the test unless the house is all there, and gone unless it is none of it.
func (h house) whole(t *testing.T, s store.Store, want bool) {
	t.Helper()
	se, r, i, p, o := h.present(t, s)
	if want && (se != 1 || r != 2 || i != 1 || p != 1 || o != 4) || !want && (se+r+i+p+o != 0) {
		t.Errorf("thread %s holds %d session, %d replies, %d interactions, %d presses, %d outbox rows; want it whole = %v", h.key.Thread, se, r, i, p, o, want)
	}
}

func (su suite) sessionRetention(t *testing.T) {
	cutoff := t0.Add(24 * time.Hour)
	horizons := func(b string) store.SessionHorizons {
		return store.SessionHorizons{ByBinding: map[string]time.Time{b: cutoff}, Otherwise: cutoff}
	}
	purge := func(t *testing.T, s store.Store, h store.SessionHorizons, limit int) store.PurgedSessions {
		t.Helper()
		got, err := s.PurgeExpiredSessions(t.Context(), h, limit)
		noErr(t, err, "PurgeExpiredSessions")
		return got
	}

	su.run(t, "PurgeExpiredSessions deletes a session before its horizon with every row that belongs to it, and keeps one at it", func(t *testing.T, s store.Store) {
		gone := house{keyOf("1.1"), "ns/b", cutoff.Add(-time.Microsecond)}
		edge := house{keyOf("2.2"), "ns/b", cutoff} // the horizon is a time the session must be before
		recent := house{keyOf("3.3"), "ns/b", cutoff.Add(time.Hour)}
		for _, h := range []house{gone, edge, recent} {
			h.build(t, s)
		}
		// What no session owns stays: an outbox row of a digest, a thread of another channel, the audit log.
		_, err := s.CreateOutbox(t.Context(), store.OutboxEntry{ID: "digest", EventID: "window", Role: store.OutboxDigest, Channel: "C1", CreatedAt: t0})
		noErr(t, err, "digest")
		noErr(t, s.AppendAudit(t.Context(), newAudit("a", t0)), "audit")

		if got := purge(t, s, horizons("ns/b"), 10); got != built {
			t.Fatalf("purged %+v; want %+v: one house", got, built)
		}
		gone.whole(t, s, false)
		edge.whole(t, s, true)
		recent.whole(t, s, true)
		if _, err := s.GetOutbox(t.Context(), "digest"); err != nil {
			t.Errorf("the digest row after = %v; want it kept: it belongs to no session", err)
		}
		if l, err := s.ListAudit(t.Context(), time.Time{}, 0, 10); err != nil || len(l) != 1 {
			t.Errorf("audit after = %v, %v; want its entry", l, err)
		}
		if got := purge(t, s, horizons("ns/b"), 10); got != (store.PurgedSessions{}) {
			t.Errorf("purged %+v again; want nothing", got)
		}
		// The horizon moves on: the two that were inside it are past it now, and go together.
		later := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": t0.Add(1000 * time.Hour)}, Otherwise: t0}
		if got := purge(t, s, later, 10); got.Sessions != 2 || got.Replies != 4 || got.Interactions != 2 || got.Presses != 2 || got.Outbox != 8 {
			t.Errorf("purged %+v; want two houses", got)
		}
	})

	su.run(t, "PurgeExpiredSessions gives each Binding its own horizon, and a Binding it is not told of the other one", func(t *testing.T, s store.Store) {
		long := house{keyOf("1.1"), "ns/long", t0.Add(time.Hour)}
		short := house{keyOf("2.2"), "ns/short", t0.Add(time.Hour)}
		left := house{keyOf("3.3"), "ns/left", t0.Add(time.Hour)}
		for _, h := range []house{long, short, left} {
			h.build(t, s)
		}
		h := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/long": t0, "ns/short": t0.Add(2 * time.Hour)}, Otherwise: t0.Add(2 * time.Hour)}
		if got := purge(t, s, h, 10); got.Sessions != 2 {
			t.Fatalf("purged %+v; want the short one's and the one whose Binding left", got)
		}
		long.whole(t, s, true)
		short.whole(t, s, false)
		left.whole(t, s, false)
	})

	su.run(t, "PurgeExpiredSessions with no Binding named applies the one horizon to every session", func(t *testing.T, s store.Store) {
		old, recent := house{keyOf("1.1"), "ns/a", t0}, house{keyOf("2.2"), "ns/b", t0.Add(2 * time.Hour)}
		old.build(t, s)
		recent.build(t, s)
		if got := purge(t, s, store.SessionHorizons{Otherwise: t0.Add(time.Hour)}, 10); got != built {
			t.Fatalf("purged %+v; want the one house before the horizon: %+v", got, built)
		}
		old.whole(t, s, false)
		recent.whole(t, s, true)
	})

	// The rows of a reply are those whose source starts with its ID and a "/". Another reply whose ID begins the same way
	// and goes on differently (a longer ID, or the byte before or after "/") is another's, and so are its rows.
	su.run(t, "PurgeExpiredSessions deletes the rows derived from a reply's ID and no one else's", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		gone := house{keyOf("1.1"), "ns/b", t0}
		gone.build(t, s)
		bystander := house{keyOf("2.2"), "ns/b", cutoff.Add(time.Hour)} // inside the horizon
		bystander.build(t, s)
		var others []string
		for _, suffix := range []string{"0", "-", "x"} { // the byte after "/", the one before it, a longer ID
			r := newReply(gone.id("r1")+suffix, bystander.key)
			r.EnqueuedAt = t0
			_, err := s.EnqueueReply(ctx, r)
			noErr(t, err, "enqueue "+r.ID)
			noErr(t, s.MarkReplyDelivered(ctx, r.ID, t0), "deliver "+r.ID)
			id, err := outbox.New(outbox.Config{Store: s, Now: func() time.Time { return t0 }}).
				EnqueueReaction(ctx, outbox.Reaction{Source: r.ID + "/queued", Channel: "C1", TS: "7.7", Name: "hourglass_flowing_sand"})
			noErr(t, err, "reaction of "+r.ID)
			noErr(t, s.RecordMessageRef(ctx, id, "7.7"), "post "+id)
			others = append(others, id)
		}
		if got := purge(t, s, horizons("ns/b"), 10); got.Sessions != 1 || got.Outbox != 4 {
			t.Fatalf("purged %+v; want the one house, and its 4 outbox rows", got)
		}
		for _, id := range others {
			if _, err := s.GetOutbox(ctx, id); err != nil {
				t.Errorf("the row %s of another reply after the purge = %v; want it kept", id, err)
			}
		}
	})

	su.run(t, "PurgeExpiredSessions never deletes a session with an active task, a pending interaction, a reply not delivered or an outbox row not posted", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		old := t0.Add(-10 * ttl) // far older than the horizon, which is t0
		// Each case holds a session back, and says what lets it go.
		cases := []struct {
			name          string
			hold, release func(h house)
		}{
			{"an active or paused task",
				func(h house) { noErr(t, s.SetSessionTask(ctx, h.key, "ctx", "task1", store.TaskTurn{}), "task") },
				func(h house) { noErr(t, s.SetSessionTask(ctx, h.key, "", "", store.TaskTurn{}), "task ends") }},
			{"a pending interaction",
				func(h house) {
					i := newInteraction(h.id("pending"), time.Hour)
					i.Key = h.key
					_, err := s.CreateInteraction(ctx, i)
					noErr(t, err, "pending")
				},
				func(h house) {
					noErr(t, s.ResolveInteraction(ctx, h.id("pending"), store.InteractionCancelled, t0.Add(time.Hour)), "settle")
				}},
			{"a reply not delivered",
				func(h house) {
					r := newReply(h.id("queued"), h.key)
					r.EnqueuedAt = old
					_, err := s.EnqueueReply(ctx, r)
					noErr(t, err, "queued")
				},
				func(h house) { noErr(t, s.MarkReplyDelivered(ctx, h.id("queued"), t0), "deliver") }},
			{"an outbox row for the thread, not posted",
				func(h house) {
					_, err := s.CreateOutbox(ctx, store.OutboxEntry{ID: h.id("unposted"), EventID: "e", Role: store.OutboxReply, Channel: h.key.Channel, Thread: h.key.Thread, CreatedAt: old, Payload: "{}"})
					noErr(t, err, "unposted")
				},
				func(h house) {
					_, err := s.AbandonOutbox(ctx, h.id("unposted"), t0.Add(time.Minute))
					noErr(t, err, "abandon")
				}},
			{"an outbox row for one of its replies, not posted",
				func(h house) { h.reaction(t, s, "r2") }, // the queued reaction of the second reply, not made yet
				func(h house) {
					noErr(t, s.RecordMessageRef(ctx, outbox.ID(h.queued("r2"), store.OutboxReaction), "7.7"), "post")
				}},
		}
		held := make([]house, len(cases))
		for i, c := range cases {
			held[i] = house{keyOf(fmt.Sprintf("%d.%d", i+1, i+1)), "ns/b", old}
			held[i].build(t, s)
			c.hold(held[i])
		}
		// A session with none of it goes, so that a held one is not kept by accident of its age or Binding.
		free := house{keyOf("99.99"), "ns/b", old}
		free.build(t, s)

		h := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": t0}, Otherwise: t0}
		if got := purge(t, s, h, 100); got != built {
			t.Fatalf("purged %+v; want only the free session: %+v", got, built)
		}
		free.whole(t, s, false)
		for i, c := range cases {
			held[i].whole(t, s, true)
			c.release(held[i])
			if got := purge(t, s, h, 100); got.Sessions != 1 {
				t.Errorf("purged %+v once %s was resolved; want that session", got, c.name)
			}
			held[i].whole(t, s, false)
		}
		// The reaction that was held back and then made is deleted with the rest: found by its source, not by a thread.
		last := held[len(held)-1]
		if _, err := s.GetOutbox(ctx, outbox.ID(last.queued("r2"), store.OutboxReaction)); !isNotFound(err) {
			t.Errorf("the reaction row of the second reply after its session was deleted = %v; want it gone", err)
		}
	})

	su.run(t, "PurgeExpiredSessions deletes at most the limit, the oldest first", func(t *testing.T, s store.Store) {
		var hs []house
		for i := range 5 {
			hs = append(hs, house{keyOf(fmt.Sprintf("%d.%d", i+1, i+1)), "ns/b", t0.Add(time.Duration(4-i) * time.Hour)}) // the later the thread, the older
		}
		for _, h := range hs {
			h.build(t, s)
		}
		h := horizons("ns/b")
		if got := purge(t, s, h, 2); got.Sessions != 2 || got.Replies != 4 || got.Outbox != 8 {
			t.Fatalf("first purge = %+v; want 2 sessions and what belongs to them", got)
		}
		hs[4].whole(t, s, false) // t0 and t0 + 1 h
		hs[3].whole(t, s, false)
		hs[2].whole(t, s, true)
		if got := purge(t, s, h, 2); got.Sessions != 2 {
			t.Fatalf("second purge = %+v; want 2", got)
		}
		if got := purge(t, s, h, 2); got.Sessions != 1 {
			t.Fatalf("third purge = %+v; want the last: fewer than the limit says it is done", got)
		}
		if got := purge(t, s, h, 2); got.Sessions != 0 {
			t.Errorf("fourth purge = %+v; want nothing", got)
		}
	})

	su.run(t, "PurgeExpiredSessions refuses arguments that are not legal", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		h := house{key, "ns/b", t0}
		h.build(t, s)
		for name, c := range map[string]struct {
			h     store.SessionHorizons
			limit int
		}{
			"no limit":           {horizons("ns/b"), 0},
			"a negative limit":   {horizons("ns/b"), -1},
			"no Otherwise":       {store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": cutoff}}, 1},
			"a zero horizon":     {store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": {}}, Otherwise: cutoff}, 1},
			"no horizons at all": {store.SessionHorizons{}, 1},
		} {
			_, err := s.PurgeExpiredSessions(ctx, c.h, c.limit)
			isErr(t, err, store.ErrInvalid, name)
		}
		h.whole(t, s, true)
	})
}
