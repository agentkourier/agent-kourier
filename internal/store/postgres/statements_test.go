package postgres

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// squash is q with its white space collapsed, so that a statement reads as one line.
func squash(q string) string { return strings.Join(strings.Fields(q), " ") }

// The statements that carry a policy predicate, as text, without a server: the predicates are numbered after the
// statement's own arguments, and what the adapter keeps is the shape of the statement.
func TestStatementsNumberTheirArgumentsInTheOrderTheyAreBound(t *testing.T) {
	horizons := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/a": t0}, Otherwise: t0.Add(time.Hour)}
	task := store.StaleTask{Key: store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}, TaskID: "task1"}

	query, args := endStaleTaskQuery(horizons, task)
	if want := `UPDATE sessions s SET task_id = '', turn_id = '', turn_user = '', turn_task = '', turn_message_ts = '', turn_acknowledgment = '', turn_acknowledgment_id = '' WHERE s.connection = $1 AND s.channel = $2 AND s.thread_ts = $3 AND s.task_id = $4 ` +
		`AND s.last_activity < $5::timestamptz AND s.last_activity < CASE s.binding WHEN $6::text THEN $7::timestamptz ELSE $8::timestamptz END`; squash(query) != want {
		t.Errorf("endStaleTaskQuery = %s; want %s", squash(query), want)
	}
	if want := []any{"slack", "C1", "1.1", "task1", t0.Add(time.Hour), "ns/a", t0, t0.Add(time.Hour)}; !reflect.DeepEqual(args, want) {
		t.Errorf("endStaleTaskQuery arguments = %v; want %v", args, want)
	}

	// The rows are locked, and those that a writer holds are skipped.
	query, args = expiredSessionsQuery(horizons, 7)
	if got := squash(query); !strings.HasPrefix(got, `SELECT s.connection, s.channel, s.thread_ts FROM sessions s WHERE s.last_activity < $1::timestamptz AND s.last_activity < CASE s.binding WHEN $2::text THEN $3::timestamptz ELSE $4::timestamptz END AND s.task_id = '' AND NOT EXISTS (`) ||
		!strings.HasSuffix(got, `ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT $5 FOR UPDATE OF s SKIP LOCKED`) {
		t.Errorf("expiredSessionsQuery = %s", got)
	}
	if want := []any{t0.Add(time.Hour), "ns/a", t0, t0.Add(time.Hour), 7}; !reflect.DeepEqual(args, want) {
		t.Errorf("expiredSessionsQuery arguments = %v; want %v", args, want)
	}

	// $4 is the opening time of the new row, the same for opened and last_seen.
	query, args = openTriggerThreadQuery(
		store.TriggerThread{Binding: "ns/a", KeyHash: "h", Thread: "1.1", Opened: t0}, store.TriggerStale{Idle: t0.Add(time.Minute)})
	if got := squash(query); !strings.Contains(got, `VALUES ($1, $2, $3, $4, $4, NULL, NULL) ON CONFLICT (binding, key_hash) DO UPDATE SET`) ||
		!strings.HasSuffix(got, `trigger_threads.last_seen < $6)) RETURNING `+triggerThreadColumns) {
		t.Errorf("openTriggerThreadQuery = %s", got)
	}
	if want := []any{"ns/a", "h", "1.1", t0, nil, t0.Add(time.Minute)}; !reflect.DeepEqual(args, want) {
		t.Errorf("openTriggerThreadQuery arguments = %v; want %v", args, want)
	}
}

// The purge deletes in a statement for each table over all the keys, in an order that deletes what names a reply before the
// reply, and only the settled rows: the number of statements is what keeps a batch under the statement timeout.
func TestPurgeStepsAreOneStatementForEachTableInOrder(t *testing.T) {
	var out store.PurgedSessions
	steps := purgeSteps(&out)
	var got []string
	for _, s := range steps {
		got = append(got, s.what)
	}
	want := []string{"presses", "interactions", "outbox rows of the threads", "outbox rows of the replies", "replies", "sessions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("purge steps = %v; want %v", got, want)
	}
	byWhat := map[string]string{}
	for _, s := range steps {
		byWhat[s.what] = squash(s.query)
	}
	const settled = `(o.message_ref <> '' OR o.abandoned_at IS NOT NULL)`
	for what, fragment := range map[string]string{
		"presses":                    `WHERE i.state <> 'pending')`,
		"interactions":               `AND i.state <> 'pending'`,
		"outbox rows of the threads": `AND ` + settled,
		"outbox rows of the replies": `WHERE o.event_id COLLATE "C" >= rr.id || '/' AND o.event_id COLLATE "C" < rr.id || '0' AND ` + settled,
		"replies":                    `AND r.delivered_at IS NOT NULL`,
	} {
		if !strings.HasSuffix(byWhat[what], fragment) {
			t.Errorf("purge of the %s = %s; want it to end in %s", what, byWhat[what], fragment)
		}
	}
	// Each step counts into its own total.
	counted := map[string]*int{"presses": &out.Presses, "interactions": &out.Interactions, "outbox rows of the threads": &out.Outbox,
		"outbox rows of the replies": &out.Outbox, "replies": &out.Replies, "sessions": &out.Sessions}
	for _, s := range steps {
		if s.n != counted[s.what] {
			t.Errorf("the %s are counted into the wrong total", s.what)
		}
	}
}
