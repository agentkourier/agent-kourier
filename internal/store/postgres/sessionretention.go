package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.SessionRetention = (*adapter)(nil)

// PurgeExpiredSessions implements store.SessionRetention. It is a write like any other: the epoch is checked first, so
// only the leader purges. The sessions are chosen and locked (a session that a writer holds is left for the next call)
// and everything is deleted in one transaction, a statement for each table over all the keys, so a batch is a few
// statements and none is longer than the statement timeout allows for the batch the caller sized.
func (s *adapter) PurgeExpiredSessions(ctx context.Context, h store.SessionHorizons, limit int) (store.PurgedSessions, error) {
	var out store.PurgedSessions
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		conns, channels, threads, err := expiredSessions(ctx, tx, h, limit)
		if err != nil || len(threads) == 0 {
			return err
		}
		// Only settled rows are deleted: each statement reads its own snapshot (READ COMMITTED), so a row that was
		// written after the sessions were chosen, an unposted notice for a reply in the thread, is not one the sweep
		// found nothing holding back. The rows that name the session's replies go before the replies.
		for _, d := range purgeSteps(&out) {
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

// purgeStep is one delete of the purge of sessions: what it deletes, the statement, and the count it adds to. Each statement
// takes the keys of the sessions as three parallel arrays, $1 to $3, and is for all of them at once.
type purgeStep struct {
	what  string
	query string
	n     *int
}

// purgeSteps are the deletes of the purge, in the order they run, adding what they delete to out. Only settled rows are
// deleted (store.SettledOutbox and the rest), and a statement is for all the keys, so a batch is a few statements and none
// is longer than the statement timeout allows for the batch the caller sized.
func purgeSteps(out *store.PurgedSessions) []purgeStep {
	const keys = `unnest($1::text[], $2::text[], $3::text[]) AS k(connection, channel, thread_ts)`
	d := Dialect{}
	decided, settled := store.DecidedInteractions("i").Text(d), store.SettledOutbox("o").Text(d)
	return []purgeStep{
		{"presses", `DELETE FROM interaction_presses WHERE interaction_id IN
				(SELECT i.id FROM interactions i JOIN ` + keys + ` ON i.connection = k.connection AND i.channel = k.channel AND i.thread_ts = k.thread_ts
				WHERE ` + decided + `)`, &out.Presses},
		{"interactions", `DELETE FROM interactions i USING ` + keys + ` WHERE i.connection = k.connection AND i.channel = k.channel AND i.thread_ts = k.thread_ts
				AND ` + decided, &out.Interactions},
		{"outbox rows of the threads", `DELETE FROM outbox o USING ` + keys + ` WHERE o.channel = k.channel AND o.thread_ts = k.thread_ts
				AND ` + settled, &out.Outbox},
		// An outbox row is tied to a reply that has no thread of its own by its source, in a range of the reply's ID
		// (store.OutboxDerivedFromReplyRow).
		{"outbox rows of the replies", `DELETE FROM outbox o USING
				(SELECT r.id FROM reply_queue r JOIN ` + keys + ` ON r.connection = k.connection AND r.channel = k.channel AND r.thread_ts = k.thread_ts) AS rr
				WHERE ` + store.OutboxDerivedFromReplyRow("o", "rr").Text(d) + ` AND ` + settled, &out.Outbox},
		{"replies", `DELETE FROM reply_queue r USING ` + keys + ` WHERE r.connection = k.connection AND r.channel = k.channel AND r.thread_ts = k.thread_ts
				AND ` + store.DeliveredReplies("r").Text(d), &out.Replies},
		{"sessions", `DELETE FROM sessions s USING ` + keys + ` WHERE s.connection = k.connection AND s.channel = k.channel AND s.thread_ts = k.thread_ts`, &out.Sessions},
	}
}

// expiredSessionsQuery is the statement that finds, and locks, the sessions that are past their horizon and that nothing
// holds back, oldest first, and its arguments. A session that a writer holds is skipped, for the next call.
func expiredSessionsQuery(h store.SessionHorizons, limit int) (string, []any) {
	b := store.NewBinder(Dialect{})
	past := b.Render(store.PastHorizon("s", h))
	held := b.Render(store.NothingHoldsBack("s"))
	lim := b.Arg(limit)
	return `SELECT s.connection, s.channel, s.thread_ts FROM sessions s
		WHERE ` + past + ` AND ` + held + `
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT ` + lim + `
		FOR UPDATE OF s SKIP LOCKED`, b.Args()
}

// expiredSessions finds, and locks, the sessions that are past their horizon and that nothing holds back, oldest first,
// and returns the three parts of their keys as parallel slices.
func expiredSessions(ctx context.Context, tx pgx.Tx, h store.SessionHorizons, limit int) (conns, channels, threads []string, err error) {
	query, args := expiredSessionsQuery(h, limit)
	rows, err := tx.Query(ctx, query, args...)
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
