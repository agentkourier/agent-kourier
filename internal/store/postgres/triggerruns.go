package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// day is the window of maxRunsPerDay, and so how long the rows of trigger_runs are of use.
const day = 24 * time.Hour

// runningStartsQuery is the statement that counts the admitted starts of a trigger since a time whose investigation is
// still going: its session has an active task, or it has a turn that is queued and not yet delivered, which is a start
// that was taken and has not begun. A run counts once however many of its turns are queued.
func runningStartsQuery() string {
	return `SELECT COUNT(*) FROM trigger_runs r
	WHERE r.binding = $1 AND r.trigger_index = $2 AND r.limited = '' AND r.at > $3
	AND (EXISTS (SELECT 1 FROM sessions s WHERE s.connection = r.connection AND s.channel = r.channel AND s.thread_ts = r.thread_ts AND s.task_id <> '')
	  OR EXISTS (SELECT 1 FROM reply_queue q WHERE q.connection = r.connection AND q.channel = r.channel AND q.thread_ts = r.thread_ts AND ` + store.UndeliveredReplies("q").Text(Dialect{}) + `))`
}

// startsSince counts the admitted starts of a trigger after a time. Its arguments are the Binding, the trigger and the time.
const startsSince = `SELECT COUNT(*) FROM trigger_runs WHERE binding = $1 AND trigger_index = $2 AND limited = '' AND at > $3`

// openWindow finds the digest window that a stopped start of the trigger joins: the open one, which has not ended and
// whose digest is not queued. Its arguments are the Binding, the trigger and the time of the start.
const openWindow = `SELECT window_start, window_end FROM trigger_runs
	WHERE binding = $1 AND trigger_index = $2 AND limited <> '' AND NOT digested AND window_end > $3 ORDER BY window_start LIMIT 1`

// dueWindows lists the stopped starts of the windows that ended and are not digested, with their counts. Its argument
// is now.
const dueWindows = `SELECT binding, trigger_index, connection, channel, window_start, window_end, limited, COUNT(*)
	FROM trigger_runs WHERE limited <> '' AND NOT digested AND window_end <= $1
	GROUP BY binding, trigger_index, connection, channel, window_start, window_end, limited
	ORDER BY window_start, binding, trigger_index, limited`

// markDigested marks a window digested. Its arguments are the Binding, the trigger and the start of the window.
const markDigested = `UPDATE trigger_runs SET digested = true
	WHERE binding = $1 AND trigger_index = $2 AND window_start = $3 AND limited <> '' AND NOT digested`

// AdmitTriggerRun implements store.TriggerRuns. The checks, the window of a stopped start and the write are one
// transaction under an advisory lock that every admission takes, so two starts cannot both pass the last free place:
// the limits are counts, and READ COMMITTED would let two callers read the same count. A start is brief and the lock is
// held for it only.
func (s *adapter) AdmitTriggerRun(ctx context.Context, run store.TriggerRun, limits store.TriggerRunLimits) (store.TriggerAdmission, error) {
	var admission store.TriggerAdmission
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, lockTriggerRuns, ""); err != nil {
			return err
		}
		var previous string
		stoppedBefore := true
		switch err := tx.QueryRow(ctx, `SELECT limited FROM trigger_runs WHERE connection = $1 AND channel = $2 AND thread_ts = $3`,
			run.Key.Connection, run.Key.Channel, run.Key.Thread).Scan(&previous); {
		case errors.Is(err, pgx.ErrNoRows):
			stoppedBefore = false
		case err != nil:
			return err
		case previous == "":
			admission = store.TriggerAdmission{Admitted: true} // asked before, and admitted: not a second start
			return nil
		}
		limit, err := reachedLimit(ctx, tx, run, limits)
		switch {
		case err != nil:
			return err
		case limit == "":
			admission = store.TriggerAdmission{Admitted: true}
			return recordStart(ctx, tx, run)
		}
		admission = store.TriggerAdmission{Limit: limit}
		if stoppedBefore {
			return nil // its row and window stand
		}
		return recordStopped(ctx, tx, run, limit, limits.DigestWindow)
	})
	if err != nil {
		return store.TriggerAdmission{}, fmt.Errorf("admit trigger run of %q: %w", run.Binding, err)
	}
	return admission, nil
}

// reachedLimit returns the first limit that the trigger has reached as of run.At, in the order daily, rate, concurrent, or
// the empty limit.
func reachedLimit(ctx context.Context, tx pgx.Tx, run store.TriggerRun, limits store.TriggerRunLimits) (store.TriggerLimit, error) {
	started := func(since time.Time) (n int, err error) {
		err = tx.QueryRow(ctx, startsSince, run.Binding, run.Trigger, since).Scan(&n)
		return n, err
	}
	if limits.MaxPerDay > 0 {
		if n, err := started(run.At.Add(-day)); err != nil || n >= limits.MaxPerDay {
			return store.LimitDaily, err
		}
	}
	if limits.MaxRuns > 0 && limits.Per > 0 {
		if n, err := started(run.At.Add(-limits.Per)); err != nil || n >= limits.MaxRuns {
			return store.LimitRate, err
		}
	}
	if limits.MaxConcurrent > 0 {
		var n int
		if err := tx.QueryRow(ctx, runningStartsQuery(), run.Binding, run.Trigger, run.At.Add(-day)).Scan(&n); err != nil || n >= limits.MaxConcurrent {
			return store.LimitConcurrent, err
		}
	}
	return "", nil
}

// recordStart records the start, over a row of the message that was stopped before.
func recordStart(ctx context.Context, tx pgx.Tx, run store.TriggerRun) error {
	_, err := tx.Exec(ctx, `INSERT INTO trigger_runs (connection, channel, thread_ts, binding, trigger_index, at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (connection, channel, thread_ts) DO UPDATE SET binding = EXCLUDED.binding, trigger_index = EXCLUDED.trigger_index,
			at = EXCLUDED.at, limited = '', window_start = NULL, window_end = NULL, digested = false`,
		run.Key.Connection, run.Key.Channel, run.Key.Thread, run.Binding, run.Trigger, run.At)
	return err
}

// recordStopped records that a limit stopped the start, in the trigger's open digest window, or in a new one that this
// start opens. A window is open while it has not ended and its digest is not queued.
func recordStopped(ctx context.Context, tx pgx.Tx, run store.TriggerRun, limit store.TriggerLimit, window time.Duration) error {
	start, end := run.At, run.At.Add(window)
	err := tx.QueryRow(ctx, openWindow, run.Binding, run.Trigger, run.At).Scan(&start, &end)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO trigger_runs (connection, channel, thread_ts, binding, trigger_index, at, limited, window_start, window_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		run.Key.Connection, run.Key.Channel, run.Key.Thread, run.Binding, run.Trigger, run.At, string(limit), start, end)
	return err
}

// ListTriggerDigests implements store.TriggerRuns.
func (s *adapter) ListTriggerDigests(ctx context.Context, now time.Time) ([]store.TriggerDigest, error) {
	rows, err := s.pool.Query(ctx, dueWindows, now)
	if err != nil {
		return nil, fmt.Errorf("list trigger digests: %w", err)
	}
	defer rows.Close()
	var out []store.TriggerDigest
	for rows.Next() {
		var d store.TriggerDigest
		var start, end time.Time
		var limit string
		var n int
		if err := rows.Scan(&d.Binding, &d.Trigger, &d.Connection, &d.Channel, &start, &end, &limit, &n); err != nil {
			return nil, fmt.Errorf("list trigger digests: %w", err)
		}
		d.Start, d.End = utc(start), utc(end)
		if i := len(out) - 1; i >= 0 && out[i].Binding == d.Binding && out[i].Trigger == d.Trigger && out[i].Start.Equal(d.Start) {
			out[i].Counts[store.TriggerLimit(limit)] += n
			continue
		}
		d.Counts = map[store.TriggerLimit]int{store.TriggerLimit(limit): n}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trigger digests: %w", err)
	}
	return out, nil
}

// MarkTriggerDigested implements store.TriggerRuns.
func (s *adapter) MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) error {
	if _, err := s.exec(ctx, markDigested, binding, trigger, start); err != nil {
		return fmt.Errorf("mark trigger digested of %q: %w", binding, err)
	}
	return nil
}

// PurgeTriggerRuns implements store.TriggerRuns.
func (s *adapter) PurgeTriggerRuns(ctx context.Context, before time.Time) (int, error) {
	n, err := s.exec(ctx, `DELETE FROM trigger_runs WHERE at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge trigger runs: %w", err)
	}
	return int(n), nil
}
