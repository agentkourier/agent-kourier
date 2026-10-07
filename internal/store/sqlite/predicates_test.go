package sqlite

import (
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// The policy predicates of package store are rendered in SQLite's Dialect and evaluated here against rows, as the
// statements of the adapter use them. The conformance suite proves what the operations do with them; these prove what
// each predicate selects.

// selectIDs runs a SELECT of one text column and returns the column, in order.
func selectIDs(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.a.read.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// outboxRow inserts a raw outbox row: posted if messageRef is set, abandoned if abandonedAt is.
func outboxRow(t *testing.T, s *Store, id, eventID, messageRef string, abandonedAt int64) {
	t.Helper()
	if _, err := s.a.write.ExecContext(t.Context(), `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at, message_ref, abandoned_at)
		VALUES (?, ?, 'root', 'C1', '1.1', 1, ?, ?)`, id, eventID, messageRef, abandonedAt); err != nil {
		t.Fatalf("insert outbox row %s: %v", id, err)
	}
}

func TestPendingAndSettledOutboxPartitionTheRows(t *testing.T) {
	s := openTemp(t)
	outboxRow(t, s, "pending", "e1", "", 0)
	outboxRow(t, s, "posted", "e2", "ts.1", 0)
	outboxRow(t, s, "abandoned", "e3", "", 5)
	outboxRow(t, s, "posted-then-abandoned", "e4", "ts.2", 6)
	outboxRow(t, s, "negative-abandoned", "e5", "", -7) // not a time the store writes, but neither pending nor settled would orphan it
	d := Dialect{}
	for _, c := range []struct {
		name string
		cond store.Fixed
		want []string
	}{
		{"PendingOutbox", store.PendingOutbox(""), []string{"pending"}},
		{"SettledOutbox", store.SettledOutbox(""), []string{"abandoned", "negative-abandoned", "posted", "posted-then-abandoned"}},
		{"aliased PendingOutbox", store.PendingOutbox("o"), []string{"pending"}},
	} {
		query := `SELECT id FROM outbox o WHERE ` + c.cond.Text(d) + ` ORDER BY id`
		if got := selectIDs(t, s, query); !slices.Equal(got, c.want) {
			t.Errorf("%s selects %v; want %v", c.name, got, c.want)
		}
	}
}

// The outbox rows of a reply with no thread are those whose source is the reply's ID, a "/" and a suffix: not the bare
// ID, and not another ID that the ID is the start of.
func TestOutboxDerivedFromReplyIDSelectsExactlyThePrefixRange(t *testing.T) {
	s := openTemp(t)
	for _, event := range []string{"r1/queued", "r1/notice", "r1/a/b", "r1", "r10", "r10/queued", "r2/queued", "r", "r0"} {
		outboxRow(t, s, event+"#row", event, "", 0)
	}
	b := store.NewBinder(Dialect{})
	derived := b.Render(store.OutboxDerivedFromReplyID("", "r1"))
	got := selectIDs(t, s, `SELECT event_id FROM outbox WHERE `+derived+` ORDER BY event_id`, b.Args()...)
	if want := []string{"r1/a/b", "r1/notice", "r1/queued"}; !slices.Equal(got, want) {
		t.Errorf("OutboxDerivedFromReplyID(r1) selects %v; want %v", got, want)
	}
}

func TestPastHorizonSelectsTheSessionsOfEachBindingBeforeItsOwnHorizon(t *testing.T) {
	s := openTemp(t)
	for _, c := range []struct {
		thread, binding string
		last            time.Time
	}{
		{"a-old", "ns/a", t0.Add(-3 * time.Hour)},    // before a's horizon: past
		{"a-new", "ns/a", t0.Add(-90 * time.Minute)}, // after a's horizon: not
		{"b-old", "ns/b", t0.Add(-90 * time.Minute)}, // before b's horizon: past
		{"b-new", "ns/b", t0.Add(-10 * time.Minute)}, // after b's horizon: not
		{"gone-old", "ns/gone", t0.Add(-5 * time.Hour)},
		{"gone-new", "ns/gone", t0.Add(-3 * time.Hour)},
	} {
		k := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: c.thread}
		if err := s.UpsertSession(t.Context(), store.Session{Key: k, ContextID: "c", Binding: c.binding, LastActivity: c.last}); err != nil {
			t.Fatalf("UpsertSession %s: %v", c.thread, err)
		}
	}
	h := store.SessionHorizons{
		ByBinding: map[string]time.Time{"ns/a": t0.Add(-2 * time.Hour), "ns/b": t0.Add(-time.Hour)},
		Otherwise: t0.Add(-4 * time.Hour), // a Binding that left the config
	}
	b := store.NewBinder(Dialect{})
	past := b.Render(store.PastHorizon("s", h))
	got := selectIDs(t, s, `SELECT s.thread_ts FROM sessions s WHERE `+past+` ORDER BY s.thread_ts`, b.Args()...)
	if want := []string{"a-old", "b-old", "gone-old"}; !slices.Equal(got, want) {
		t.Errorf("PastHorizon selects %v; want %v", got, want)
	}
}

// A closed thread whose cooldown never ran is past it, and a cutoff that is not in effect is before no row.
func TestTriggerThreadPurgeableSelectsTheRowsThatDoNothing(t *testing.T) {
	s := openTemp(t)
	for _, r := range []struct {
		thread                         string
		lastSeen, closedAt, cooldownTo int64
	}{
		{"open-idle", 10, 0, 7},           // open, last seen before the cutoff
		{"open-recent", 90, 0, 7},         // open, seen after it
		{"closed-no-cooldown", 90, 20, 0}, // closed, no cooldown ran: past it
		{"closed-cooled", 90, 20, 30},     // closed, cooldown ended before now
		{"closed-cooling", 90, 20, 200},   // closed, cooldown still runs
	} {
		if _, err := s.a.write.ExecContext(t.Context(), `INSERT INTO trigger_threads (binding, key_hash, thread, opened, last_seen, closed_at, cooldown_until)
			VALUES ('ns/b', ?, ?, 1, ?, ?, ?)`, r.thread, r.thread, r.lastSeen, r.closedAt, r.cooldownTo); err != nil {
			t.Fatalf("insert %s: %v", r.thread, err)
		}
	}
	now, idle := time.Unix(0, 100), time.Unix(0, 50)
	for _, c := range []struct {
		name       string
		idleBefore time.Time
		want       []string
	}{
		{"with a cutoff", idle, []string{"closed-cooled", "closed-no-cooldown", "open-idle"}},
		{"with none in effect", time.Time{}, []string{"closed-cooled", "closed-no-cooldown"}},
		// The year 1000 does not fit SQLite's nanoseconds, and wraps to a positive number: a cutoff of it would be after every row.
		{"with one long before the epoch", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), []string{"closed-cooled", "closed-no-cooldown"}},
	} {
		b := store.NewBinder(Dialect{})
		purgeable := b.Render(store.TriggerThreadPurgeable(now, c.idleBefore))
		got := selectIDs(t, s, `SELECT thread FROM trigger_threads WHERE `+purgeable+` ORDER BY thread`, b.Args()...)
		if !slices.Equal(got, c.want) {
			t.Errorf("TriggerThreadPurgeable %s selects %v; want %v", c.name, got, c.want)
		}
	}
}
