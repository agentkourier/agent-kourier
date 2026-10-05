package storetest

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// fidelity is what a store gives back of what it was given: times as the same instant in UTC to the
// microsecond, text as the same text, bytes as the same bytes.
func (su suite) fidelity(t *testing.T) {
	// A zone and a microsecond that a store which keeps milliseconds, or a local time, would show. Times finer than
	// a microsecond are not pinned (a store may truncate or round them), so none is used.
	zone := time.FixedZone("UTC+5:30", 5*3600+1800)
	at := time.Date(2026, 9, 29, 17, 30, 0, 123456000, zone) // 12:00:00.123456 UTC

	want := func(t *testing.T, what string, got, instant time.Time) {
		t.Helper()
		if !got.Equal(instant) {
			t.Errorf("%s = %v (%d µs); want the instant %v (%d µs)", what, got, got.UnixMicro(), instant, instant.UnixMicro())
		}
		if got.Location() != time.UTC {
			t.Errorf("%s is in %v; want UTC", what, got.Location())
		}
	}
	wantUnset := func(t *testing.T, what string, got time.Time) {
		t.Helper()
		if !got.IsZero() {
			t.Errorf("%s = %v; want the zero time for a time never set", what, got)
		}
	}

	su.run(t, "the times of an event, a claim and a session are the same instants, in UTC, to the microsecond", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		_, err := s.AppendEvent(ctx, newEvent("ev", at))
		noErr(t, err, "AppendEvent")
		got := getEvent(t, s, "ev")
		want(t, "ReceivedAt", got.ReceivedAt, at)
		want(t, "NextAttemptAt of a new event", got.NextAttemptAt, at)
		lease := time.Hour + 7*time.Microsecond
		claimed, err := s.ClaimEvent(ctx, at, lease)
		noErr(t, err, "ClaimEvent")
		want(t, "the lease deadline of the claim", claimed.NextAttemptAt, at.Add(lease))
		want(t, "ReceivedAt of the claim", claimed.ReceivedAt, at)
		want(t, "the lease deadline as stored", getEvent(t, s, "ev").NextAttemptAt, at.Add(lease))
		noErr(t, s.FailEvent(ctx, "ev", 1, at.Add(3*time.Microsecond), 5, "x"), "FailEvent")
		want(t, "the retry time", getEvent(t, s, "ev").NextAttemptAt, at.Add(3*time.Microsecond))

		sess := store.Session{Key: key, TaskID: "t", Binding: "ns/b", LastActivity: at}
		noErr(t, s.UpsertSession(ctx, sess), "UpsertSession")
		gs, err := s.GetSession(ctx, key, ttl, at)
		noErr(t, err, "GetSession")
		want(t, "LastActivity", gs.LastActivity, at)
		noErr(t, s.TouchSession(ctx, key, at.Add(time.Microsecond)), "TouchSession")
		gs, _ = s.GetSession(ctx, key, ttl, at)
		want(t, "LastActivity after a touch", gs.LastActivity, at.Add(time.Microsecond))
		active, err := s.ListActiveSessions(ctx)
		if err != nil || len(active) != 1 {
			t.Fatalf("ListActiveSessions = %+v, %v", active, err)
		}
		want(t, "LastActivity as listed", active[0].LastActivity, at.Add(time.Microsecond))
	})

	su.run(t, "the times of a reply, an outbox row and an audit entry are the same instants, in UTC", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		r := newReply("r", key)
		r.EnqueuedAt = at
		_, err := s.EnqueueReply(ctx, r)
		noErr(t, err, "EnqueueReply")
		got, err := s.NextReply(ctx, key)
		noErr(t, err, "NextReply")
		want(t, "EnqueuedAt", got.EnqueuedAt, at)

		addOutbox(t, s, "o", at, "{}")
		addOutbox(t, s, "gone", at, "{}")
		want(t, "CreatedAt", getOutbox(t, s, "o").CreatedAt, at)
		want(t, "CreatedAt as listed", unposted(t, s)[0].CreatedAt, at)
		_, err = s.AbandonOutbox(ctx, "gone", at.Add(time.Microsecond))
		noErr(t, err, "AbandonOutbox")
		want(t, "AbandonedAt", getOutbox(t, s, "gone").AbandonedAt, at.Add(time.Microsecond))
		wantUnset(t, "AbandonedAt of a row not abandoned", getOutbox(t, s, "o").AbandonedAt)

		noErr(t, s.AppendAudit(ctx, newAudit("s", at)), "AppendAudit")
		l, err := s.ListAudit(ctx, at, 0, 10) // since in a zone of its own: the same instant is included
		if err != nil || len(l) != 1 {
			t.Fatalf("ListAudit since the instant = %+v, %v; want the entry", l, err)
		}
		want(t, "At", l[0].At, at)
		if l, _ = s.ListAudit(ctx, at.Add(time.Microsecond), 0, 10); len(l) != 0 {
			t.Errorf("ListAudit a microsecond after the entry = %+v; want none", l)
		}
	})

	su.run(t, "the times of an interaction and a press are the same instants, in UTC; those never set are zero", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		i := newInteraction("i", time.Hour)
		i.CreatedAt, i.Deadline = at, at.Add(time.Hour+time.Microsecond)
		_, err := s.CreateInteraction(ctx, i)
		noErr(t, err, "CreateInteraction")
		got := getInteraction(t, s, "i")
		want(t, "CreatedAt", got.CreatedAt, at)
		want(t, "Deadline", got.Deadline, i.Deadline)
		wantUnset(t, "DecidedAt of a pending interaction", got.DecidedAt)
		wantUnset(t, "DeliveredAt of a pending interaction", got.DeliveredAt)
		noErr(t, s.SubmitInteraction(ctx, "i", []byte("y"), "U1", at.Add(5*time.Microsecond)), "SubmitInteraction")
		got = getInteraction(t, s, "i")
		want(t, "DecidedAt", got.DecidedAt, at.Add(5*time.Microsecond))
		wantUnset(t, "DeliveredAt of an answer not yet delivered", got.DeliveredAt)
		noErr(t, s.MarkInteractionDelivered(ctx, "i", at.Add(9*time.Microsecond)), "MarkInteractionDelivered")
		want(t, "DeliveredAt", getInteraction(t, s, "i").DeliveredAt, at.Add(9*time.Microsecond))
		// The same instant in another zone asks the same question.
		if l, err := s.ListDecidedInteractions(ctx, at.Add(5*time.Microsecond)); err != nil || len(l) != 1 {
			t.Errorf("ListDecidedInteractions since the decision = %+v, %v; want it", l, err)
		}

		_, err = s.RecordPress(ctx, press("i", "U1", at), roomy)
		noErr(t, err, "RecordPress")
		ps, err := s.ListPresses(ctx)
		if err != nil || len(ps) != 1 {
			t.Fatalf("ListPresses = %+v, %v", ps, err)
		}
		want(t, "PressedAt", ps[0].PressedAt, at)
		if got, err := s.PendingPresses(ctx, "i", at); err != nil || !got {
			t.Errorf("PendingPresses at the instant of the press = %v, %v; want true", got, err)
		}
		if got, err := s.PendingPresses(ctx, "i", at.Add(-time.Microsecond)); err != nil || got {
			t.Errorf("PendingPresses a microsecond before = %v, %v; want false", got, err)
		}
	})

	su.run(t, "the times of a trigger thread and a digest are the same instants, in UTC; those never set are zero", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		th, opened, err := reserve(t, s, opening("100.1", at))
		if err != nil || !opened {
			t.Fatalf("OpenTriggerThread = %v, %v", opened, err)
		}
		want(t, "Opened", th.Opened, at)
		want(t, "LastSeen", th.LastSeen, at)
		wantUnset(t, "ClosedAt of an open thread", th.ClosedAt)
		wantUnset(t, "CooldownUntil of a thread not started", th.CooldownUntil)
		noErr(t, s.MarkTriggerStarted(ctx, tBinding, tKey, "100.1", at.Add(tCool+time.Microsecond)), "MarkTriggerStarted")
		_, err = s.CloseTriggerThread(ctx, tBinding, tKey, at.Add(2*time.Microsecond))
		noErr(t, err, "CloseTriggerThread")
		th, err = s.GetTriggerThread(ctx, tBinding, tKey)
		noErr(t, err, "GetTriggerThread")
		want(t, "ClosedAt", th.ClosedAt, at.Add(2*time.Microsecond))
		want(t, "LastSeen after the close", th.LastSeen, at.Add(2*time.Microsecond))
		want(t, "CooldownUntil", th.CooldownUntil, at.Add(tCool+time.Microsecond))

		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: 10 * time.Minute}
		admit(t, s, runOf("1.0", at), lim)
		admit(t, s, runOf("1.1", at.Add(time.Second+time.Microsecond)), lim)
		ds, err := s.ListTriggerDigests(ctx, at.Add(time.Hour))
		if err != nil || len(ds) != 1 {
			t.Fatalf("ListTriggerDigests = %+v, %v", ds, err)
		}
		want(t, "digest Start", ds[0].Start, at.Add(time.Second+time.Microsecond))
		want(t, "digest End", ds[0].End, at.Add(time.Second+time.Microsecond+10*time.Minute))
	})

	// The far past and the far future that a clock reads in tests and in rows that outlive a deployment.
	su.run(t, "instants far from now come back exact", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for i, instant := range []time.Time{
			time.Date(1971, 1, 1, 0, 0, 0, 1000, time.UTC),
			time.Date(2200, 12, 31, 23, 59, 59, 999999000, time.UTC),
		} {
			id := fmt.Sprint("ev", i)
			_, err := s.AppendEvent(ctx, newEvent(id, instant))
			noErr(t, err, "AppendEvent")
			want(t, "ReceivedAt", getEvent(t, s, id).ReceivedAt, instant)
			noErr(t, s.AppendAudit(ctx, newAudit(id, instant)), "AppendAudit")
		}
		l, err := s.ListAudit(ctx, time.Time{}, 0, 10)
		if err != nil || len(l) != 2 {
			t.Fatalf("ListAudit = %+v, %v", l, err)
		}
		want(t, "audit At", l[0].At, time.Date(1971, 1, 1, 0, 0, 0, 1000, time.UTC))
		want(t, "audit At", l[1].At, time.Date(2200, 12, 31, 23, 59, 59, 999999000, time.UTC))
	})

	su.run(t, "a time given in another zone is the same instant in a query", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		appendEvent(t, s, "ev", t0)
		other := t0.Add(time.Minute).In(zone)
		if got, err := s.ClaimEvent(ctx, other, time.Minute); err != nil || got.ID != "ev" {
			t.Errorf("claim at %v = %+v, %v; want the event", other, got, err)
		}
		noErr(t, s.UpsertSession(ctx, store.Session{Key: key, Binding: "ns/b", LastActivity: t0}), "UpsertSession")
		if _, err := s.GetSession(ctx, key, time.Hour, t0.Add(time.Hour).In(zone)); err != nil {
			t.Errorf("a session exactly the ttl old, asked in another zone = %v; want live", err)
		}
		_, err := s.GetSession(ctx, key, time.Hour, t0.Add(time.Hour+time.Microsecond).In(zone))
		isErr(t, err, store.ErrExpired, "a session a microsecond too old, asked in another zone")
		createInteraction(t, s, "i")
		if l, err := s.ListDueInteractions(ctx, t0.Add(time.Hour).In(zone)); err != nil || len(l) != 1 {
			t.Errorf("due at the deadline, asked in another zone = %+v, %v; want the interaction", l, err)
		}
	})

	// Text is data: IDs and values are compared exactly, as bytes, and never folded, trimmed or normalised.
	su.run(t, "text is compared exactly: case, spaces, accents and quotes make different IDs", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		ids := []string{
			"ev", "EV", " ev", "ev ", "ev\t", "évé", "évé", "ev'", `ev"`, `ev\`, "ev%", "ev_", "e_", "ev;--",
			"x'; DROP TABLE events; --", "日本語", "🚨", strings.Repeat("long-", 100),
		}
		for _, id := range ids {
			ok, err := s.AppendEvent(ctx, newEvent(id, t0))
			if err != nil || !ok {
				t.Errorf("AppendEvent(%q) = %v, %v; want a row of its own", id, ok, err)
			}
		}
		for _, id := range ids {
			if got := getEvent(t, s, id); got.ID != id {
				t.Errorf("GetEvent(%q) = %q", id, got.ID)
			}
		}
		_, err := s.GetEvent(ctx, "Ev")
		isErr(t, err, store.ErrNotFound, "GetEvent of an ID that differs only in case")
	})

	su.run(t, "bytes are opaque: every value, and a megabyte, round-trip", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		all := make([]byte, 256)
		for i := range all {
			all[i] = byte(i)
		}
		big := bytes.Repeat([]byte{0, 0xff, 'a', '\n'}, 256*1024)
		e := newEvent("ev", t0)
		e.Payload = big
		_, err := s.AppendEvent(ctx, e)
		noErr(t, err, "AppendEvent")
		if got := getEvent(t, s, "ev"); !bytes.Equal(got.Payload, big) {
			t.Errorf("event payload: %d bytes back of %d", len(got.Payload), len(big))
		}
		i := newInteraction("i", time.Hour)
		i.Request = all
		_, err = s.CreateInteraction(ctx, i)
		noErr(t, err, "CreateInteraction")
		noErr(t, s.SubmitInteraction(ctx, "i", all, "U1", t0.Add(time.Minute)), "SubmitInteraction")
		if got := getInteraction(t, s, "i"); !bytes.Equal(got.Request, all) || !bytes.Equal(got.Answer, all) {
			t.Errorf("interaction request or answer changed: %x / %x", got.Request, got.Answer)
		}
		p := press("i", "U1", t0)
		p.Inputs = all
		_, err = s.RecordPress(ctx, p, roomy)
		noErr(t, err, "RecordPress")
		if got, _ := s.ListPresses(ctx); len(got) != 1 || !bytes.Equal(got[0].Inputs, all) {
			t.Errorf("press inputs changed: %+v", got)
		}
	})

	// The Seq of a press names it to DeletePress, so a seq that was given to a processed press and then to a new
	// one would let the first press's retry delete the second.
	su.run(t, "the seq of a press that was processed is never given to another", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		first, err := s.RecordPress(ctx, press("ix", "U1", t0), roomy)
		noErr(t, err, "RecordPress")
		noErr(t, s.DeletePress(ctx, first), "DeletePress")
		second, err := s.RecordPress(ctx, press("ix", "U2", t0), roomy)
		noErr(t, err, "RecordPress")
		if second <= first {
			t.Errorf("a press recorded after the first was deleted has seq %d; want above %d", second, first)
		}
		noErr(t, s.DeletePress(ctx, first), "a retried DeletePress")
		if got, _ := s.ListPresses(ctx); len(got) != 1 || got[0].Seq != second {
			t.Errorf("presses = %+v; want the second, untouched by the retry", got)
		}
	})
}
