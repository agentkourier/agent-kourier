package sqlite

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// squash is q with its white space collapsed, so that a statement reads as one line.
func squash(q string) string { return strings.Join(strings.Fields(q), " ") }

// The arguments of a statement are bound in the order of its placeholders: positional placeholders have no other way
// to tell them apart.
func TestStatementsBindTheirArgumentsInTheOrderOfTheirPlaceholders(t *testing.T) {
	horizons := store.SessionHorizons{ByBinding: map[string]time.Time{"ns/a": t0}, Otherwise: t0.Add(time.Hour)}
	task := store.StaleTask{Key: key, TaskID: "task1"}
	stale := store.TriggerStale{Unstarted: t0, Idle: t0.Add(time.Minute)}
	open := store.TriggerThread{Binding: "ns/a", KeyHash: "h", Thread: "1.1", Opened: t0}
	query, args := endStaleTaskQuery(horizons, task)
	if want := `UPDATE sessions AS s SET task_id = '', turn_id = '', turn_user = '', turn_task = '', turn_message_ts = '', turn_acknowledgment = '', turn_acknowledgment_id = '' WHERE s.connection = ? AND s.channel = ? AND s.thread_ts = ? AND s.task_id = ? ` +
		`AND s.last_activity < ? AND s.last_activity < CASE s.binding WHEN ? THEN ? ELSE ? END`; squash(query) != want {
		t.Errorf("endStaleTaskQuery = %s; want %s", squash(query), want)
	}
	if want := []any{"slack", "C1", "1.1", "task1", ts(t0.Add(time.Hour)), "ns/a", ts(t0), ts(t0.Add(time.Hour))}; !reflect.DeepEqual(args, want) {
		t.Errorf("endStaleTaskQuery arguments = %v; want %v", args, want)
	}

	query, args = expiredSessionsQuery(horizons, 7)
	if got := squash(query); !strings.HasPrefix(got, `SELECT s.connection, s.channel, s.thread_ts FROM sessions s WHERE s.last_activity < ? AND s.last_activity < CASE s.binding WHEN ? THEN ? ELSE ? END AND s.task_id = '' AND NOT EXISTS (`) ||
		!strings.HasSuffix(got, `ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT ?`) {
		t.Errorf("expiredSessionsQuery = %s", got)
	}
	if want := []any{ts(t0.Add(time.Hour)), "ns/a", ts(t0), ts(t0.Add(time.Hour)), 7}; !reflect.DeepEqual(args, want) {
		t.Errorf("expiredSessionsQuery arguments = %v; want %v", args, want)
	}

	query, args = openTriggerThreadQuery(open, stale)
	if got := squash(query); !strings.Contains(got, `VALUES (?, ?, ?, ?, ?, 0, 0) ON CONFLICT (binding, key_hash) DO UPDATE SET`) || !strings.HasSuffix(got, `trigger_threads.last_seen < ?))`) {
		t.Errorf("openTriggerThreadQuery = %s", got)
	}
	if want := []any{"ns/a", "h", "1.1", ts(t0), ts(t0), ts(t0), ts(t0.Add(time.Minute))}; !reflect.DeepEqual(args, want) {
		t.Errorf("openTriggerThreadQuery arguments = %v; want %v", args, want)
	}
}
