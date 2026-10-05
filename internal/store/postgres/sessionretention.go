package postgres

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

var _ store.SessionRetention = (*Store)(nil)

// settledOutbox is the outbox rows that are done with: posted, or given up on. A row that is neither may still be
// delivered, and is not the sweep's to delete (it holds its session back).
const settledOutbox = `(o.message_ref <> '' OR o.abandoned_at IS NOT NULL)`

// PurgeExpiredSessions implements store.SessionRetention. It is a write like any other: the epoch is checked first, so
// only the leader purges. The sessions are chosen and locked (a session that a writer holds is left for the next call)
// and everything is deleted in one transaction, a statement for each table over all the keys, so a batch is a few
// statements and none is longer than the statement timeout allows for the batch the caller sized.
func (s *Store) PurgeExpiredSessions(ctx context.Context, h store.SessionHorizons, limit int) (purged store.PurgedSessions, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgeExpiredSessions")
	defer func() { telemetryErr = s.finish(op, telemetryErr) }()
	if !h.Valid() || limit < 1 || limit > math.MaxInt32 {
		return store.PurgedSessions{}, fmt.Errorf("purge expired sessions: every horizon and a limit of 1 to %d are required: %w", math.MaxInt32, store.ErrInvalid)
	}
	var out store.PurgedSessions
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		conns, channels, threads, err := expiredSessions(ctx, tx, h, limit)
		if err != nil || len(threads) == 0 {
			return err
		}
		// Only settled rows are deleted: each statement reads its own snapshot (READ COMMITTED), so a row that was
		// written after the sessions were chosen, an unposted notice for a reply in the thread, is not one the sweep
		// found nothing holding back. The rows that name the session's replies go before the replies.
		const keys = `unnest($1::text[], $2::text[], $3::text[]) AS k(connection, channel, thread_ts)`
		for _, d := range []struct {
			what  string
			query string
			n     *int
		}{
			{"presses", `DELETE FROM interaction_presses WHERE interaction_id IN
				(SELECT i.id FROM interactions i JOIN ` + keys + ` ON i.connection = k.connection AND i.channel = k.channel AND i.thread_ts = k.thread_ts
				WHERE i.state <> 'pending')`, &out.Presses},
			{"interactions", `DELETE FROM interactions i USING ` + keys + ` WHERE i.connection = k.connection AND i.channel = k.channel AND i.thread_ts = k.thread_ts
				AND i.state <> 'pending'`, &out.Interactions},
			{"outbox rows of the threads", `DELETE FROM outbox o USING ` + keys + ` WHERE o.channel = k.channel AND o.thread_ts = k.thread_ts
				AND ` + settledOutbox, &out.Outbox},
			// An outbox row is tied to a reply that has no thread of its own like this: a row made for a reply has the reply's ID
			// and a "/" and a suffix as its source (event_id): the queued reaction of a turn, and the removal of it, are made for
			// Source "<reply ID>/queued" (session.queuedID), and the runner's notices for "<reply ID>/<kind>" (Manager.notice).
			// Such a row has no thread, so this is the only way to find it. Its source is in the range from id + "/" up to, and
			// not including, id + "0", the character after "/": a range of the outbox_event index (migration 0022), compared in
			// the C collation, in which the range is exactly the prefix (a locale's order ignores punctuation). The range
			// deliberately excludes the bare ID: a row sourced by it is a render post made for the thread, which the
			// (channel, thread) delete above finds.
			{"outbox rows of the replies", `DELETE FROM outbox o USING
				(SELECT r.id FROM reply_queue r JOIN ` + keys + ` ON r.connection = k.connection AND r.channel = k.channel AND r.thread_ts = k.thread_ts) AS rr
				WHERE o.event_id COLLATE "C" >= rr.id || '/' AND o.event_id COLLATE "C" < rr.id || '0' AND ` + settledOutbox, &out.Outbox},
			{"replies", `DELETE FROM reply_queue r USING ` + keys + ` WHERE r.connection = k.connection AND r.channel = k.channel AND r.thread_ts = k.thread_ts
				AND r.delivered_at IS NOT NULL`, &out.Replies},
			{"sessions", `DELETE FROM sessions s USING ` + keys + ` WHERE s.connection = k.connection AND s.channel = k.channel AND s.thread_ts = k.thread_ts`, &out.Sessions},
		} {
			tag, err := tx.Exec(ctx, d.query, conns, channels, threads)
			if err != nil {
				return fmt.Errorf("delete the %s: %w", d.what, err)
			}
			*d.n += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return store.PurgedSessions{}, fmt.Errorf("purge expired sessions: %w", err)
	}
	return out, nil
}

// expiredSessions finds, and locks, the sessions that are past their horizon and that nothing holds back, oldest first,
// and returns the three parts of their keys as parallel slices. The first condition is the latest of the horizons, which
// the sessions' index answers; the rest is the session's own.
func expiredSessions(ctx context.Context, tx pgx.Tx, h store.SessionHorizons, limit int) (conns, channels, threads []string, err error) {
	latest := h.Otherwise
	for _, at := range h.ByBinding {
		if at.After(latest) {
			latest = at
		}
	}
	args := []any{latest}
	var when strings.Builder
	for _, b := range slices.Sorted(maps.Keys(h.ByBinding)) {
		args = append(args, b, h.ByBinding[b])
		fmt.Fprintf(&when, " WHEN $%d::text THEN $%d::timestamptz", len(args)-1, len(args))
	}
	args = append(args, h.Otherwise, limit)
	horizon := fmt.Sprintf("$%d::timestamptz", len(args)-1) // a CASE needs a WHEN, so with no Binding named it is the one horizon
	if when.Len() > 0 {
		horizon = fmt.Sprintf("CASE s.binding%s ELSE %s END", when.String(), horizon)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT s.connection, s.channel, s.thread_ts FROM sessions s
		WHERE s.last_activity < $1::timestamptz
		AND s.last_activity < %s
		AND s.task_id = ''
		AND NOT EXISTS (SELECT 1 FROM interactions i WHERE i.connection = s.connection AND i.channel = s.channel
			AND i.thread_ts = s.thread_ts AND i.state = 'pending')
		AND NOT EXISTS (SELECT 1 FROM reply_queue r WHERE r.connection = s.connection AND r.channel = s.channel
			AND r.thread_ts = s.thread_ts AND r.delivered_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.channel = s.channel AND o.thread_ts = s.thread_ts
			AND o.message_ref = '' AND o.abandoned_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM reply_queue r JOIN outbox o
			ON o.event_id COLLATE "C" >= r.id || '/' AND o.event_id COLLATE "C" < r.id || '0'
			WHERE r.connection = s.connection AND r.channel = s.channel AND r.thread_ts = s.thread_ts
			AND o.message_ref = '' AND o.abandoned_at IS NULL)
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT $%d
		FOR UPDATE OF s SKIP LOCKED`, horizon, len(args)), args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c, ch, th string
		if err := rows.Scan(&c, &ch, &th); err != nil {
			return nil, nil, nil, fmt.Errorf("scan session key: %w", err)
		}
		conns, channels, threads = append(conns, c), append(channels, ch), append(threads, th)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("find sessions: %w", err)
	}
	return conns, channels, threads, nil
}
