package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var _ store.SessionRetention = (*adapter)(nil)

// PurgeExpiredSessions implements store.SessionRetention. The candidates are chosen and everything is deleted in one
// transaction on the single write connection, so no other statement runs between the choice and the delete.
func (s *adapter) PurgeExpiredSessions(ctx context.Context, h store.SessionHorizons, limit int) (store.PurgedSessions, error) {
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

// expiredSessionsQuery is the statement that finds the sessions that are past their horizon and that nothing holds back,
// oldest first, and its arguments.
func expiredSessionsQuery(h store.SessionHorizons, limit int) (string, []any) {
	b := store.NewBinder(Dialect{})
	past := b.Render(store.PastHorizon("s", h))
	held := b.Render(store.NothingHoldsBack("s"))
	lim := b.Arg(limit)
	return `SELECT s.connection, s.channel, s.thread_ts FROM sessions s
		WHERE ` + past + ` AND ` + held + `
		ORDER BY s.last_activity, s.connection, s.channel, s.thread_ts LIMIT ` + lim, b.Args()
}

// expiredSessions returns the keys of the sessions that are past their horizon and that nothing holds back, oldest
// first.
func expiredSessions(ctx context.Context, tx *sql.Tx, h store.SessionHorizons, limit int) ([]store.ConversationKey, error) {
	query, args := expiredSessionsQuery(h, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
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
	d := Dialect{}
	decided, settled := store.DecidedInteractions("").Text(d), store.SettledOutbox("").Text(d)
	steps := []purgeStep{
		{"presses", `DELETE FROM interaction_presses WHERE interaction_id IN
			(SELECT id FROM interactions WHERE connection = ? AND channel = ? AND thread_ts = ? AND ` + decided + `)`, &out.Presses, where},
		{"interactions", `DELETE FROM interactions WHERE connection = ? AND channel = ? AND thread_ts = ? AND ` + decided, &out.Interactions, where},
		{"outbox rows of the thread", `DELETE FROM outbox WHERE channel = ? AND thread_ts = ? AND ` + settled, &out.Outbox, []any{k.Channel, k.Thread}},
	}
	for _, id := range replies {
		b := store.NewBinder(d)
		derived := b.Render(store.OutboxDerivedFromReplyID("", id))
		steps = append(steps, purgeStep{"outbox rows of a reply", `DELETE FROM outbox WHERE ` + derived + ` AND ` + settled, &out.Outbox, b.Args()})
	}
	steps = append(steps, purgeStep{"replies", `DELETE FROM reply_queue WHERE connection = ? AND channel = ? AND thread_ts = ? AND ` + store.DeliveredReplies("").Text(d), &out.Replies, where})
	for _, step := range steps {
		n, err := execRows(ctx, tx, step.query, step.args...)
		if err != nil {
			return fmt.Errorf("delete the %s of %+v: %w", step.what, k, err)
		}
		*step.n += int(n)
	}
	n, err := execRows(ctx, tx, `DELETE FROM sessions WHERE connection = ? AND channel = ? AND thread_ts = ?`, where...)
	if err != nil {
		return fmt.Errorf("delete the session %+v: %w", k, err)
	}
	out.Sessions += int(n)
	return nil
}
