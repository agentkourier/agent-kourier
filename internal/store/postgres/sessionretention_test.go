package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/storetest"
)

func oldSession(t *testing.T, s *Store, thread string) store.ConversationKey {
	t.Helper()
	k := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}
	if err := s.UpsertSession(t.Context(), store.Session{Key: k, ContextID: "c", Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	return k
}

var anyHorizon = store.SessionHorizons{Otherwise: t0.Add(time.Hour)}

func sessionRows(t *testing.T, dsn string) int {
	t.Helper()
	var n int
	if err := admin(t).QueryRow(t.Context(), `SELECT count(*) FROM `+schemaOf(t, dsn)+`.sessions`).Scan(&n); err != nil {
		t.Fatalf("count the sessions: %v", err)
	}
	return n
}

// TestPurgeExpiredSessionsIsFenced: the sweep is a write of the leader like any other. A standby, and a leader that another
// has since replaced, delete nothing and are told so.
func TestPurgeExpiredSessionsIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	if _, err := a.Lead(t.Context(), "pod-a"); err != nil {
		t.Fatal(err)
	}
	oldSession(t, a, "1.1")

	if _, err := b.PurgeExpiredSessions(t.Context(), anyHorizon, 10); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a standby's purge = %v; want store.ErrDeposed", err)
	}
	if _, err := b.Lead(t.Context(), "pod-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PurgeExpiredSessions(t.Context(), anyHorizon, 10); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a deposed leader's purge = %v; want store.ErrDeposed", err)
	}
	if n := sessionRows(t, dsn); n != 1 {
		t.Errorf("%d sessions after the refused purges; want the 1 that was there", n)
	}
	got, err := b.PurgeExpiredSessions(t.Context(), anyHorizon, 10)
	if err != nil || got.Sessions != 1 {
		t.Errorf("the leader's purge = %+v, %v; want it to delete the session", got, err)
	}
}

// TestPurgeExpiredSessionsLeavesASessionAWriterHolds: a session whose row another transaction has locked is left for the
// next call, not waited for.
func TestPurgeExpiredSessionsLeavesASessionAWriterHolds(t *testing.T) {
	s, dsn := openFresh(t)
	held, free := oldSession(t, s, "1.1"), oldSession(t, s, "2.2")
	tx, err := admin(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM `+schemaOf(t, dsn)+`.sessions WHERE thread_ts = $1 FOR UPDATE`, held.Thread); err != nil {
		t.Fatal(err)
	}

	got, err := s.PurgeExpiredSessions(t.Context(), anyHorizon, 10)
	if err != nil || got.Sessions != 1 {
		t.Fatalf("purge with a session locked = %+v, %v; want only the other one, and no wait", got, err)
	}
	if _, err := s.GetSession(t.Context(), free, time.Hour, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the free session after = %v; want it gone", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PurgeExpiredSessions(t.Context(), anyHorizon, 10); err != nil || got.Sessions != 1 {
		t.Errorf("purge once it is released = %+v, %v; want the held one", got, err)
	}
}

// TestPurgeExpiredSessionsDeletesOnlySettledRows: each statement of the purge reads its own snapshot (READ COMMITTED), so
// a row that is written after the sessions were chosen is seen by the deletes that follow. The test holds the purge at its
// first delete, on a lock of its own, writes a notice that is not posted, a reply that is not delivered and a pending
// interaction for the thread, and a reaction that is not made for its delivered reply, and lets it go: the session and the
// delivered reply go, and those, with the press that is waiting on the pending interaction, which no one has settled, stay.
func TestPurgeExpiredSessionsDeletesOnlySettledRows(t *testing.T) {
	s, dsn := openFresh(t)
	k := oldSession(t, s, "1.1")
	ctx := t.Context()
	if _, err := s.EnqueueReply(ctx, store.Reply{ID: "done-reply", Key: k, UserID: "U1", MessageRef: "1.1", Text: "hi", EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReplyDelivered(ctx, "done-reply", t0); err != nil {
		t.Fatal(err)
	}
	// A press for the interaction that is asked while the purge waits: the press table is locked then, so it is recorded now.
	if _, err := s.RecordPress(ctx, store.Press{InteractionID: "late-ask", User: "U1", Channel: k.Channel, MessageRef: "1.1", PressedAt: t0}, store.PressLimits{Total: 10, PerUser: 10}); err != nil {
		t.Fatal(err)
	}
	tx, err := admin(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	if _, err := tx.Exec(t.Context(), `LOCK TABLE `+schemaOf(t, dsn)+`.interaction_presses IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		got store.PurgedSessions
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := s.PurgeExpiredSessions(t.Context(), anyHorizon, 10)
		done <- result{got, err}
	}()
	waitForWaiter(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'relation' AND NOT granted`) // the purge has chosen its sessions, and waits at the presses

	if _, err := s.CreateOutbox(ctx, store.OutboxEntry{ID: "late-notice", EventID: "late", Role: store.OutboxNotice, Channel: k.Channel, Thread: k.Thread, CreatedAt: t0, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueReply(ctx, store.Reply{ID: "late-reply", Key: k, UserID: "U1", MessageRef: "1.1", Text: "hi", EnqueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInteraction(ctx, store.Interaction{ID: "late-ask", Key: k, TaskID: "t", Kind: store.InteractionAskUser, Request: []byte(`{}`), CreatedAt: t0, Deadline: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// The queued reaction of the delivered reply, asked for and not made yet: found by its source, and not settled.
	reaction, err := storetest.EnqueueOnlyOutbox(t, s, func() time.Time { return t0 }).
		EnqueueReaction(ctx, outbox.Reaction{Source: "done-reply/queued", Channel: k.Channel, TS: "1.1", Name: "hourglass_flowing_sand"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil || r.got.Sessions != 1 || r.got.Replies != 1 || r.got.Outbox != 0 || r.got.Interactions != 0 || r.got.Presses != 0 {
		t.Fatalf("purge = %+v, %v; want the session and its delivered reply, and none of the rows written after it was chosen", r.got, r.err)
	}
	if _, err := s.GetOutbox(ctx, reaction); err != nil {
		t.Errorf("the unposted reaction after the purge = %v; want it kept", err)
	}
	if l, err := s.ListPresses(ctx); err != nil || len(l) != 1 {
		t.Errorf("the presses after the purge = %v, %v; want the one on the pending interaction", l, err)
	}
	if _, err := s.GetOutbox(ctx, "late-notice"); err != nil {
		t.Errorf("the unposted notice after the purge = %v; want it kept", err)
	}
	if _, err := s.NextReply(ctx, k); err != nil {
		t.Errorf("the undelivered reply after the purge = %v; want it kept", err)
	}
	if i, err := s.GetInteraction(ctx, "late-ask"); err != nil || i.State != store.InteractionPending {
		t.Errorf("the pending interaction after the purge = %+v, %v; want it kept", i, err)
	}
}
