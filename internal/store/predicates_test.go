package store_test

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/postgres"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// The policy predicates are rendered through each backend's own Dialect, and these tests hold the text each one gives,
// without a database. What the text does is the conformance suite's to prove (storetest), against each store.

var (
	pAt      = time.Unix(1000, 0).UTC()
	pDialect = []struct {
		name string
		d    store.Dialect
	}{{"sqlite", sqlite.Dialect{}}, {"postgres", postgres.Dialect{}}}
)

// condCase is a Cond with the text and the arguments it renders to in each dialect. A time is an int64 of nanoseconds in
// SQLite, and itself in Postgres.
type condCase struct {
	name string
	cond store.Cond
	sql  [2]string // sqlite, postgres
	args [2][]any
}

func (c condCase) check(t *testing.T) {
	t.Helper()
	for i, d := range pDialect {
		b := store.NewBinder(d.d)
		got := b.Render(c.cond)
		if got != c.sql[i] {
			t.Errorf("%s in %s:\n got %s\nwant %s", c.name, d.name, got, c.sql[i])
		}
		if want := c.args[i]; !reflect.DeepEqual(b.Args(), want) {
			t.Errorf("%s in %s: arguments = %#v; want %#v", c.name, d.name, b.Args(), want)
		}
	}
}

// ns is a time as SQLite binds it.
func ns(t time.Time) int64 { return t.UnixNano() }

func TestFixedPredicates(t *testing.T) {
	for _, c := range []struct {
		name string
		fix  store.Fixed
		sql  [2]string
	}{
		{"PendingOutbox bare", store.PendingOutbox(""), [2]string{
			`message_ref = '' AND abandoned_at = 0`, `message_ref = '' AND abandoned_at IS NULL`}},
		{"PendingOutbox aliased", store.PendingOutbox("o"), [2]string{
			`o.message_ref = '' AND o.abandoned_at = 0`, `o.message_ref = '' AND o.abandoned_at IS NULL`}},
		{"SettledOutbox", store.SettledOutbox("o"), [2]string{
			`(o.message_ref <> '' OR o.abandoned_at <> 0)`, `(o.message_ref <> '' OR o.abandoned_at IS NOT NULL)`}},
		{"UndeliveredReplies", store.UndeliveredReplies("r"), [2]string{`r.delivered_at IS NULL`, `r.delivered_at IS NULL`}},
		{"DeliveredReplies", store.DeliveredReplies(""), [2]string{`delivered_at IS NOT NULL`, `delivered_at IS NOT NULL`}},
		{"PendingInteractions", store.PendingInteractions(""), [2]string{`state = 'pending'`, `state = 'pending'`}},
		{"DecidedInteractions", store.DecidedInteractions("i"), [2]string{`i.state <> 'pending'`, `i.state <> 'pending'`}},
		{"OutboxDerivedFromReplyRow", store.OutboxDerivedFromReplyRow("o", "rr"), [2]string{
			`o.event_id >= rr.id || '/' AND o.event_id < rr.id || '0'`,
			`o.event_id COLLATE "C" >= rr.id || '/' AND o.event_id COLLATE "C" < rr.id || '0'`}},
		{"AcknowledgmentHeld", store.AcknowledgmentHeld("reply_queue"), [2]string{
			`EXISTS (SELECT 1 FROM sessions s WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel` +
				` AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> ''` +
				` AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id)))`,
			`EXISTS (SELECT 1 FROM sessions s WHERE s.connection = reply_queue.connection AND s.channel = reply_queue.channel` +
				` AND s.thread_ts = reply_queue.thread_ts AND s.task_id <> '' AND s.turn_acknowledgment <> ''` +
				` AND (s.turn_acknowledgment_id = reply_queue.id OR (s.turn_acknowledgment_id = '' AND s.turn_id = reply_queue.id)))`}},
		{"TriggerThreadOpen", store.TriggerThreadOpen(""), [2]string{`closed_at = 0`, `closed_at IS NULL`}},
		{"TriggerThreadClosed", store.TriggerThreadClosed(""), [2]string{`closed_at <> 0`, `closed_at IS NOT NULL`}},
		{"TriggerThreadUnstarted", store.TriggerThreadUnstarted("t"), [2]string{`t.cooldown_until = 0`, `t.cooldown_until IS NULL`}},
		{"NothingHoldsBack", store.NothingHoldsBack("s"), [2]string{
			`s.task_id = ''` +
				` AND NOT EXISTS (SELECT 1 FROM interactions i WHERE i.connection = s.connection AND i.channel = s.channel AND i.thread_ts = s.thread_ts AND i.state = 'pending')` +
				` AND NOT EXISTS (SELECT 1 FROM reply_queue r WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts AND r.delivered_at IS NULL)` +
				` AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.channel = s.channel AND o.thread_ts = s.thread_ts AND o.message_ref = '' AND o.abandoned_at = 0)` +
				` AND NOT EXISTS (SELECT 1 FROM reply_queue r JOIN outbox o ON o.event_id >= r.id || '/' AND o.event_id < r.id || '0'` +
				` WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts AND o.message_ref = '' AND o.abandoned_at = 0)`,
			`s.task_id = ''` +
				` AND NOT EXISTS (SELECT 1 FROM interactions i WHERE i.connection = s.connection AND i.channel = s.channel AND i.thread_ts = s.thread_ts AND i.state = 'pending')` +
				` AND NOT EXISTS (SELECT 1 FROM reply_queue r WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts AND r.delivered_at IS NULL)` +
				` AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.channel = s.channel AND o.thread_ts = s.thread_ts AND o.message_ref = '' AND o.abandoned_at IS NULL)` +
				` AND NOT EXISTS (SELECT 1 FROM reply_queue r JOIN outbox o ON o.event_id COLLATE "C" >= r.id || '/' AND o.event_id COLLATE "C" < r.id || '0'` +
				` WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts AND o.message_ref = '' AND o.abandoned_at IS NULL)`}},
	} {
		for i, d := range pDialect {
			if got := c.fix.Text(d.d); got != c.sql[i] {
				t.Errorf("%s in %s:\n got %s\nwant %s", c.name, d.name, got, c.sql[i])
			}
			// A Fixed rendered through a Binder is the same text, and binds nothing.
			b := store.NewBinder(d.d)
			if got := b.Render(c.fix); got != c.sql[i] || len(b.Args()) != 0 {
				t.Errorf("%s rendered by a Binder in %s = %q with %v; want %q and no arguments", c.name, d.name, got, b.Args(), c.sql[i])
			}
		}
	}
}

func TestPastHorizon(t *testing.T) {
	early, late := pAt.Add(-time.Hour), pAt.Add(time.Hour)
	named := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/b": late, "ns/a": early}, Otherwise: pAt}
	for _, c := range []condCase{
		{
			// With no Binding named it is the one horizon: a CASE needs a WHEN.
			"no Binding named", store.PastHorizon("s", store.SessionHorizons{Otherwise: pAt}),
			[2]string{`s.last_activity < ? AND s.last_activity < ?`, `s.last_activity < $1::timestamptz AND s.last_activity < $2::timestamptz`},
			[2][]any{{ns(pAt), ns(pAt)}, {pAt, pAt}},
		},
		{
			// The first part is the latest horizon; the Bindings are in order of their names, whatever the map's.
			"two Bindings named", store.PastHorizon("s", named),
			[2]string{
				`s.last_activity < ? AND s.last_activity < CASE s.binding WHEN ? THEN ? WHEN ? THEN ? ELSE ? END`,
				`s.last_activity < $1::timestamptz AND s.last_activity < CASE s.binding WHEN $2::text THEN $3::timestamptz WHEN $4::text THEN $5::timestamptz ELSE $6::timestamptz END`},
			[2][]any{{ns(late), "ns/a", ns(early), "ns/b", ns(late), ns(pAt)}, {late, "ns/a", early, "ns/b", late, pAt}},
		},
		{
			"Otherwise the latest", store.PastHorizon("x", store.SessionHorizons{ByBinding: map[string]time.Time{"ns/a": early}, Otherwise: late}),
			[2]string{
				`x.last_activity < ? AND x.last_activity < CASE x.binding WHEN ? THEN ? ELSE ? END`,
				`x.last_activity < $1::timestamptz AND x.last_activity < CASE x.binding WHEN $2::text THEN $3::timestamptz ELSE $4::timestamptz END`},
			[2][]any{{ns(late), "ns/a", ns(early), ns(late)}, {late, "ns/a", early, late}},
		},
	} {
		c.check(t)
	}
}

func TestPastHorizonCountsFromTheStatementsOwnArguments(t *testing.T) {
	// The statement's own arguments come first: the placeholders of the condition continue from them.
	b := store.NewBinder(postgres.Dialect{})
	first, second := b.Arg("a"), b.Arg("b")
	got := b.Render(store.PastHorizon("s", store.SessionHorizons{Otherwise: pAt}))
	if first != "$1" || second != "$2" || got != `s.last_activity < $3::timestamptz AND s.last_activity < $4::timestamptz` {
		t.Errorf("placeholders = %s, %s, %s", first, second, got)
	}
	if want := []any{"a", "b", pAt, pAt}; !reflect.DeepEqual(b.Args(), want) {
		t.Errorf("arguments = %v; want %v", b.Args(), want)
	}
}

func TestClearableInteractions(t *testing.T) {
	grace, undelivered := time.Hour, 3*time.Hour
	c := condCase{
		"ClearableInteractions", store.ClearableInteractions(pAt, grace, undelivered),
		[2]string{
			`state <> 'pending' AND decided_at <> 0 AND (length(request) > 0 OR length(answer) > 0) AND decided_at <= ? AND (state <> 'answered' OR delivered_at <> 0 OR decided_at <= ?)`,
			`state <> 'pending' AND decided_at IS NOT NULL AND (octet_length(request) > 0 OR octet_length(answer) > 0) AND decided_at <= $1 AND (state <> 'answered' OR delivered_at IS NOT NULL OR decided_at <= $2)`},
		[2][]any{{ns(pAt.Add(-grace)), ns(pAt.Add(-undelivered))}, {pAt.Add(-grace), pAt.Add(-undelivered)}},
	}
	c.check(t)
}

func TestOutboxDerivedFromReplyID(t *testing.T) {
	// The range is from id + "/" up to, not including, id + "0": the bare ID is not in it.
	c := condCase{
		"OutboxDerivedFromReplyID", store.OutboxDerivedFromReplyID("o", "r1"),
		[2]string{`o.event_id >= ? AND o.event_id < ?`, `o.event_id COLLATE "C" >= $1::text AND o.event_id COLLATE "C" < $2::text`},
		[2][]any{{"r1/", "r10"}, {"r1/", "r10"}},
	}
	c.check(t)
}

func TestTriggerThreadReplaceable(t *testing.T) {
	const sqliteText = `((trigger_threads.closed_at <> 0 AND (trigger_threads.cooldown_until = 0 OR trigger_threads.cooldown_until <= excluded.opened))` +
		` OR (trigger_threads.cooldown_until = 0 AND trigger_threads.thread <> excluded.thread AND trigger_threads.opened < ?)` +
		` OR (trigger_threads.closed_at = 0 AND trigger_threads.cooldown_until <> 0 AND trigger_threads.last_seen < ?))`
	const postgresText = `((trigger_threads.closed_at IS NOT NULL AND (trigger_threads.cooldown_until IS NULL OR trigger_threads.cooldown_until <= excluded.opened))` +
		` OR (trigger_threads.cooldown_until IS NULL AND trigger_threads.thread <> excluded.thread AND trigger_threads.opened < $1)` +
		` OR (trigger_threads.closed_at IS NULL AND trigger_threads.cooldown_until IS NOT NULL AND trigger_threads.last_seen < $2))`
	idle := pAt.Add(time.Hour)
	for _, c := range []condCase{
		{"both cutoffs", store.TriggerThreadReplaceable(store.TriggerStale{Unstarted: pAt, Idle: idle}),
			[2]string{sqliteText, postgresText}, [2][]any{{ns(pAt), ns(idle)}, {pAt, idle}}},
		// A cutoff that is not in effect is one no column is before: 0 where a time never set is 0, NULL where it is NULL.
		{"no cutoff", store.TriggerThreadReplaceable(store.TriggerStale{}),
			[2]string{sqliteText, postgresText}, [2][]any{{int64(0), int64(0)}, {nil, nil}}},
		{"cutoffs at or before the epoch", store.TriggerThreadReplaceable(store.TriggerStale{Unstarted: time.Unix(0, 0), Idle: time.Unix(-5, 0)}),
			[2]string{sqliteText, postgresText}, [2][]any{{int64(0), int64(0)}, {nil, nil}}},
	} {
		c.check(t)
	}
}

func TestTriggerThreadPurgeable(t *testing.T) {
	idle := pAt.Add(-time.Minute)
	for _, c := range []condCase{
		{"with idleBefore", store.TriggerThreadPurgeable(pAt, idle),
			[2]string{
				`((closed_at <> 0 AND (cooldown_until = 0 OR cooldown_until < ?)) OR (closed_at = 0 AND last_seen < ?))`,
				`((closed_at IS NOT NULL AND (cooldown_until IS NULL OR cooldown_until < $1)) OR (closed_at IS NULL AND last_seen < $2))`},
			[2][]any{{ns(pAt), ns(idle)}, {pAt, idle}}},
		{"no idleBefore deletes no open row", store.TriggerThreadPurgeable(pAt, time.Time{}),
			[2]string{
				`((closed_at <> 0 AND (cooldown_until = 0 OR cooldown_until < ?)) OR (closed_at = 0 AND last_seen < ?))`,
				`((closed_at IS NOT NULL AND (cooldown_until IS NULL OR cooldown_until < $1)) OR (closed_at IS NULL AND last_seen < $2))`},
			[2][]any{{ns(pAt), int64(0)}, {pAt, nil}}},
	} {
		c.check(t)
	}
}

// TestPredicatesAreSafeToJoin: an adapter joins a condition to the rest of its WHERE with AND, so no condition may leave
// an OR at its top level.
func TestPredicatesAreSafeToJoin(t *testing.T) {
	horizons := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/a": pAt}, Otherwise: pAt}
	for name, c := range map[string]store.Cond{
		"PastHorizon":               store.PastHorizon("s", horizons),
		"NothingHoldsBack":          store.NothingHoldsBack("s"),
		"ClearableInteractions":     store.ClearableInteractions(pAt, time.Hour, 2*time.Hour),
		"TriggerThreadReplaceable":  store.TriggerThreadReplaceable(store.TriggerStale{}),
		"TriggerThreadPurgeable":    store.TriggerThreadPurgeable(pAt, pAt),
		"SettledOutbox":             store.SettledOutbox("o"),
		"OutboxDerivedFromReplyRow": store.OutboxDerivedFromReplyRow("o", "r"),
	} {
		for _, d := range pDialect {
			text := store.NewBinder(d.d).Render(c)
			depth := 0
			for i, r := range text {
				switch {
				case r == '(':
					depth++
				case r == ')':
					depth--
				case depth == 0 && strings.HasPrefix(text[i:], " OR "):
					t.Errorf("%s in %s has an OR outside its parentheses: %s", name, d.name, text)
				}
			}
		}
	}
}

func TestCutoffIsInEffectOnlyAfterTheEpochAndNeverLaterThanTheLatestTime(t *testing.T) {
	last := time.Unix(0, math.MaxInt64)
	for _, c := range []struct {
		name     string
		at       time.Time
		inEffect bool
		bound    time.Time // the time it binds, when in effect
	}{
		{"the zero time", time.Time{}, false, time.Time{}},
		{"the epoch", time.Unix(0, 0), false, time.Time{}},
		{"before the epoch", time.Unix(-1, 0), false, time.Time{}},
		{"just after the epoch", time.Unix(0, 1), true, time.Unix(0, 1)},
		{"an ordinary time", pAt, true, pAt},
		{"the latest time", last, true, last},
		// A later time would wrap in SQLite's 64 bits to an earlier one, and take over nothing: it is the latest instead.
		{"after the latest time", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), true, last},
	} {
		for _, d := range pDialect {
			b := store.NewBinder(d.d)
			b.Cutoff(c.at)
			want := d.d.NoCutoff()
			if c.inEffect {
				want = d.d.TimeArg(c.bound)
			}
			if got := b.Args()[0]; got != want {
				t.Errorf("%s: Cutoff in %s binds %v; want %v", c.name, d.name, got, want)
			}
		}
	}
}
