package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// day is the window of maxRunsPerDay, and so how long the rows of trigger_runs are of use.
const day = 24 * time.Hour

// runningStarts counts the admitted starts of a trigger since a time whose investigation is still going: its session
// has an active task, or it has a turn that is queued and not yet delivered, which is a start that was taken and has
// not begun. A run counts once however many of its turns are queued.
const runningStarts = `SELECT COUNT(*) FROM trigger_runs r
	WHERE r.binding = ? AND r.trigger_index = ? AND r.limited = '' AND r.at > ?
	AND (EXISTS (SELECT 1 FROM sessions s WHERE s.connection = r.connection AND s.channel = r.channel AND s.thread_ts = r.thread_ts AND s.task_id <> '')
	  OR EXISTS (SELECT 1 FROM reply_queue q WHERE q.connection = r.connection AND q.channel = r.channel AND q.thread_ts = r.thread_ts AND q.delivered_at IS NULL))`

// AdmitTriggerRun implements store.TriggerRuns. The checks, the window of a stopped start and the write are one
// transaction on the write pool's single connection, so two starts cannot both pass the last free place.
func (s *Store) AdmitTriggerRun(ctx context.Context, run store.TriggerRun, limits store.TriggerRunLimits) (telemetryResult0 store.TriggerAdmission, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.AdmitTriggerRun")
	defer func() { finishStore(op, telemetryErr) }()
	if run.Binding == "" || run.Trigger < 0 || !validKey(run.Key) || !triggerTime(run.At) || limits.DigestWindow <= 0 {
		return store.TriggerAdmission{}, fmt.Errorf("admit trigger run of %q: the Binding, trigger, key, a time after the Unix epoch and a digest window are required: %w", run.Binding, store.ErrInvalid)
	}
	now := ts(run.At)
	var admission store.TriggerAdmission
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var previous string
		stoppedBefore := true
		switch err := tx.QueryRowContext(ctx, `SELECT limited FROM trigger_runs WHERE connection = ? AND channel = ? AND thread_ts = ?`,
			run.Key.Connection, run.Key.Channel, run.Key.Thread).Scan(&previous); {
		case errors.Is(err, sql.ErrNoRows):
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
			admission.Admitted = true
			return recordStart(ctx, tx, run, now)
		}
		admission.Limit = limit
		if stoppedBefore {
			return nil // its row and window stand
		}
		return recordStopped(ctx, tx, run, limit, now, int64(limits.DigestWindow))
	})
	if err != nil {
		return store.TriggerAdmission{}, fmt.Errorf("admit trigger run of %q: %w", run.Binding, err)
	}
	return admission, nil
}

// reachedLimit returns the first limit that the trigger has reached as of run.At, in the order daily, rate, concurrent, or
// the empty limit.
func reachedLimit(ctx context.Context, tx *sql.Tx, run store.TriggerRun, limits store.TriggerRunLimits) (store.TriggerLimit, error) {
	now := ts(run.At)
	started := func(since int64) (n int, err error) {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trigger_runs WHERE binding = ? AND trigger_index = ? AND limited = '' AND at > ?`,
			run.Binding, run.Trigger, since).Scan(&n)
		return n, err
	}
	if limits.MaxPerDay > 0 {
		if n, err := started(now - int64(day)); err != nil || n >= limits.MaxPerDay {
			return store.LimitDaily, err
		}
	}
	if limits.MaxRuns > 0 && limits.Per > 0 {
		if n, err := started(now - int64(limits.Per)); err != nil || n >= limits.MaxRuns {
			return store.LimitRate, err
		}
	}
	if limits.MaxConcurrent > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, runningStarts, run.Binding, run.Trigger, now-int64(day)).Scan(&n); err != nil || n >= limits.MaxConcurrent {
			return store.LimitConcurrent, err
		}
	}
	return "", nil
}

// recordStart records the start, over a row of the message that was stopped before.
func recordStart(ctx context.Context, tx *sql.Tx, run store.TriggerRun, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO trigger_runs (connection, channel, thread_ts, binding, trigger_index, at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (connection, channel, thread_ts) DO UPDATE SET binding = excluded.binding, trigger_index = excluded.trigger_index,
			at = excluded.at, limited = '', window_start = 0, window_end = 0, digested = 0`,
		run.Key.Connection, run.Key.Channel, run.Key.Thread, run.Binding, run.Trigger, now)
	return err
}

// recordStopped records that a limit stopped the start, in the trigger's open digest window, or in a new one that this
// start opens. A window is open while it has not ended and its digest is not queued.
func recordStopped(ctx context.Context, tx *sql.Tx, run store.TriggerRun, limit store.TriggerLimit, now, window int64) error {
	start, end := now, now+window
	err := tx.QueryRowContext(ctx, `SELECT window_start, window_end FROM trigger_runs
		WHERE binding = ? AND trigger_index = ? AND limited <> '' AND digested = 0 AND window_end > ? ORDER BY window_start LIMIT 1`,
		run.Binding, run.Trigger, now).Scan(&start, &end)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trigger_runs (connection, channel, thread_ts, binding, trigger_index, at, limited, window_start, window_end)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.Key.Connection, run.Key.Channel, run.Key.Thread, run.Binding, run.Trigger, now, string(limit), start, end)
	return err
}

// ReleaseTriggerRun implements store.TriggerRuns.
func (s *Store) ReleaseTriggerRun(ctx context.Context, key store.ConversationKey) (telemetryResult0 bool, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ReleaseTriggerRun")
	defer func() { finishStore(op, telemetryErr) }()
	if !validKey(key) {
		return false, fmt.Errorf("release trigger run %+v: the key is incomplete: %w", key, store.ErrInvalid)
	}
	n, err := s.exec(ctx, `DELETE FROM trigger_runs WHERE connection = ? AND channel = ? AND thread_ts = ? AND limited = ''`, key.Connection, key.Channel, key.Thread)
	if err != nil {
		return false, fmt.Errorf("release trigger run %+v: %w", key, err)
	}
	return n > 0, nil
}

// ListTriggerDigests implements store.TriggerRuns.
func (s *Store) ListTriggerDigests(ctx context.Context, now time.Time) (telemetryResult0 []store.TriggerDigest, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.ListTriggerDigests")
	defer func() { finishStore(op, telemetryErr) }()
	if !triggerTime(now) {
		return nil, fmt.Errorf("list trigger digests: the time must be after the Unix epoch: %w", store.ErrInvalid)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT binding, trigger_index, connection, channel, window_start, window_end, limited, COUNT(*)
		FROM trigger_runs WHERE limited <> '' AND digested = 0 AND window_end <= ?
		GROUP BY binding, trigger_index, connection, channel, window_start, window_end, limited
		ORDER BY window_start, binding, trigger_index, limited`, ts(now))
	if err != nil {
		return nil, fmt.Errorf("list trigger digests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.TriggerDigest
	for rows.Next() {
		var d store.TriggerDigest
		var start, end int64
		var limit string
		var n int
		if err := rows.Scan(&d.Binding, &d.Trigger, &d.Connection, &d.Channel, &start, &end, &limit, &n); err != nil {
			return nil, fmt.Errorf("list trigger digests: %w", err)
		}
		d.Start, d.End = fromTS(start), fromTS(end)
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
func (s *Store) MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) (telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.MarkTriggerDigested")
	defer func() { finishStore(op, telemetryErr) }()
	if binding == "" || trigger < 0 || !triggerTime(start) {
		return fmt.Errorf("mark trigger digested of %q: the Binding, trigger and a window start after the Unix epoch are required: %w", binding, store.ErrInvalid)
	}
	if _, err := s.exec(ctx, `UPDATE trigger_runs SET digested = 1 WHERE binding = ? AND trigger_index = ? AND window_start = ? AND limited <> ''`, binding, trigger, ts(start)); err != nil {
		return fmt.Errorf("mark trigger digested of %q: %w", binding, err)
	}
	return nil
}

// PurgeTriggerRuns implements store.TriggerRuns.
func (s *Store) PurgeTriggerRuns(ctx context.Context, before time.Time) (telemetryResult0 int, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "courier.store.PurgeTriggerRuns")
	defer func() { finishStore(op, telemetryErr) }()
	if !triggerTime(before) {
		return 0, fmt.Errorf("purge trigger runs: the time must be after the Unix epoch: %w", store.ErrInvalid)
	}
	n, err := s.exec(ctx, `DELETE FROM trigger_runs WHERE at < ?`, ts(before))
	if err != nil {
		return 0, fmt.Errorf("purge trigger runs: %w", err)
	}
	return int(n), nil
}
