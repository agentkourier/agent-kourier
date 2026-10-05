package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

var _ store.SessionRetention = (*Store)(nil)

// PurgeExpiredSessions implements store.SessionRetention. The candidates are chosen and everything is deleted in one
// transaction on the single write connection, so no other statement runs between the choice and the delete.
func (s *Store) PurgeExpiredSessions(ctx context.Context, h store.SessionHorizons, limit int) (telemetryResult0 store.PurgedSessions, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgeExpiredSessions")
	defer func() { finishStore(op, telemetryErr) }()
	if !h.Valid() || limit < 1 {
		return store.PurgedSessions{}, fmt.Errorf("purge expired sessions: every horizon and a positive limit are required: %w", store.ErrInvalid)
	}
	var out store.PurgedSessions
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		keys, err := expiredSessions(ctx, tx, h, limit)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := deleteSession(ctx, tx, k, &out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return store.PurgedSessions{}, fmt.Errorf("purge expired sessions: %w", err)
	}
	return out, nil
}

// expiredSessions returns the keys of the sessions that are past their horizon and that nothing holds back, oldest
// first. The first condition is the latest of the horizons, which the sessions' index answers; the rest is the
// session's own.
func expiredSessions(ctx context.Context, tx *sql.Tx, h store.SessionHorizons, limit int) ([]store.ConversationKey, error) {
	bindings := slices.Sorted(maps.Keys(h.ByBinding))
	latest := h.Otherwise
	for _, at := range h.ByBinding {
		latest = maxTime(latest, at)
	}
	args := []any{ts(latest)}
	var when strings.Builder
	for _, b := range bindings {
		when.WriteString(" WHEN ? THEN ?")
		args = append(args, b, ts(h.ByBinding[b]))
	}
	args = append(args, ts(h.Otherwise), limit)
	horizon := "?" // a CASE needs a WHEN, so with no Binding named it is the one horizon
	if when.Len() > 0 {
		horizon = "CASE s.binding" + when.String() + " ELSE ? END"
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.connection, s.channel, s.thread_ts FROM sessions s
		WHERE s.last_activity < ?
		AND s.last_activity < `+horizon+`
		AND s.task_id = ''
		AND NOT EXISTS (SELECT 1 FROM interactions i WHERE i.connection = s.connection AND i.channel = s.channel
			AND i.thread_ts = s.thread_ts AND i.state = 'pending')
		AND NOT EXISTS (SELECT 1 FROM reply_queue r WHERE r.connection = s.connection AND r.channel = s.channel
			AND r.thread_ts = s.thread_ts AND r.delivered_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.channel = s.channel AND o.thread_ts = s.thread_ts
			AND o.message_ref = '' AND o.abandoned_at = 0)
		AND NOT EXISTS (SELECT 1 FROM reply_queue r JOIN outbox o ON o.event_id >= r.id || '/' AND o.event_id < r.id || '0'
			WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts
			AND o.message_ref = '' AND o.abandoned_at = 0)
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("find sessions: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var keys []store.ConversationKey
	for rows.Next() {
		var k store.ConversationKey
		if err := rows.Scan(&k.Connection, &k.Channel, &k.Thread); err != nil {
			return nil, fmt.Errorf("scan session key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find sessions: %w", err)
	}
	return keys, nil
}

// derivedSources is what ties an outbox row to a reply that has no thread of its own: a row made for a reply has the
// reply's ID, a "/" and a suffix as its source (EventID): the queued reaction of a turn, and the removal of
// it, are made for Source "<reply ID>/queued" (session.queuedID), and the runner's notices for "<reply ID>/<kind>"
// (Manager.notice). Such a row has no thread, so this is the only way to find it. The rows whose source starts with
// id + "/" are those from id + "/" up to, and not including, id + "0", the character after "/", which is a range of
// the outbox_event index (migration 0022): the range holds under SQLite's binary order, and the Postgres store
// compares in the C collation to the same effect. The range deliberately excludes the bare reply ID: a row sourced by
// it is a render post made for the thread, which the (channel, thread) delete finds.
const derivedSources = `event_id >= ? AND event_id < ?`

// settledOutbox is the outbox rows that are done with: posted, or given up on. A row that is neither may still be
// delivered, and is not the sweep's to delete (it holds its session back).
const settledOutbox = `(message_ref <> '' OR abandoned_at <> 0)`

// replyIDs returns the IDs of the replies of the session k.
func replyIDs(ctx context.Context, tx *sql.Tx, k store.ConversationKey) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM reply_queue WHERE connection = ? AND channel = ? AND thread_ts = ?`, k.Connection, k.Channel, k.Thread)
	if err != nil {
		return nil, fmt.Errorf("find the replies of %+v: %w", k, err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read failures
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a reply ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find the replies of %+v: %w", k, err)
	}
	return ids, nil
}

// purgeStep is one delete of a session's purge: what it deletes, the statement and its arguments, and the count it adds to.
type purgeStep struct {
	what  string
	query string
	n     *int
	args  []any
}

// deleteSession deletes the session k and what belongs to it, adding what it deleted to out. Only settled rows are
// deleted: the sweep chose the session when nothing held it back, and a row that says otherwise is not its to delete.
// The rows that name the session's replies go before the replies.
func deleteSession(ctx context.Context, tx *sql.Tx, k store.ConversationKey, out *store.PurgedSessions) error {
	where := []any{k.Connection, k.Channel, k.Thread}
	replies, err := replyIDs(ctx, tx, k)
	if err != nil {
		return err
	}
	steps := []purgeStep{
		{"presses", `DELETE FROM interaction_presses WHERE interaction_id IN
			(SELECT id FROM interactions WHERE connection = ? AND channel = ? AND thread_ts = ? AND state <> 'pending')`, &out.Presses, where},
		{"interactions", `DELETE FROM interactions WHERE connection = ? AND channel = ? AND thread_ts = ? AND state <> 'pending'`, &out.Interactions, where},
		{"outbox rows of the thread", `DELETE FROM outbox WHERE channel = ? AND thread_ts = ? AND ` + settledOutbox, &out.Outbox, []any{k.Channel, k.Thread}},
	}
	for _, id := range replies {
		steps = append(steps, purgeStep{"outbox rows of a reply", `DELETE FROM outbox WHERE ` + derivedSources + ` AND ` + settledOutbox, &out.Outbox, []any{id + "/", id + "0"}})
	}
	steps = append(steps, purgeStep{"replies", `DELETE FROM reply_queue WHERE connection = ? AND channel = ? AND thread_ts = ? AND delivered_at IS NOT NULL`, &out.Replies, where})
	for _, d := range steps {
		n, err := execRows(ctx, tx, d.query, d.args...)
		if err != nil {
			return fmt.Errorf("delete the %s of %+v: %w", d.what, k, err)
		}
		*d.n += int(n)
	}
	n, err := execRows(ctx, tx, `DELETE FROM sessions WHERE connection = ? AND channel = ? AND thread_ts = ?`, where...)
	if err != nil {
		return fmt.Errorf("delete the session %+v: %w", k, err)
	}
	out.Sessions += int(n)
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
