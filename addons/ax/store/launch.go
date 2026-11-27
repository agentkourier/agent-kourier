package store

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	kit "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// These outcomes are stored once for each launch.
const (
	Succeeded   = "succeeded"
	CommandFail = "command_failed"
	Failed      = "failed"
	Canceled    = "canceled"
	TimedOut    = "timed_out"
	Interrupted = "interrupted"
)

// CleanupState is the persisted disposition of AX Task cleanup.
type CleanupState int

const (
	// CleanPending means the launcher has yet to confirm cleanup.
	CleanPending CleanupState = 0
	// CleanDone means AX confirmed absence or the launch had no Task.
	CleanDone CleanupState = 1
	// CleanForeign means the Task failed the ownership check.
	CleanForeign CleanupState = 2
)

// Launch records the identity of an immutable AX Task and its lifecycle.
type Launch struct {
	TaskID      string // Owning A2A task ID.
	Name        string // Deterministic AX Task name.
	Fingerprint string // Digest of the immutable Task spec.
	ResultID    string // Opaque callback correlation ID.
	UID         string // AX Task metadata UID.
	ActorUID    string // Original Substrate actor UID, once observed.
	Invocation  string // Wrapper invocation that won the start claim.
	Decision    string // Durable turn outcome, or empty while open.
	Receipt     string // Hex digest of the accepted result body.
	Output      string // Hex-encoded UTF-8 output.
	Deadline    int64  // Result deadline in Unix nanoseconds.
	DrainUntil  int64  // Reconciliation deadline in Unix nanoseconds, or zero after a definitive RPC response.
	CleanupAt   int64  // Cleanup time in Unix nanoseconds, or zero until scheduled.
	ResumeSent  int    // SQL boolean set before the first Resume RPC.
	Truncated   int    // SQL boolean indicating the wrapper truncated the output.
	Suspended   int    // SQL boolean set after AX confirms suspension.
	Clean       CleanupState
	ExitCode    int // -1 until a command result is recorded.
}

const launchColumns = `task_id, task_name, fingerprint, result_id, task_uid, actor_uid, invocation_id,
	decision, receipt, output, deadline, drain_until, cleanup_at, resume_sent, truncated, suspended, clean, exit_code`

func (l *Launch) scan(scan func(...any) error) error {
	return scan(&l.TaskID, &l.Name, &l.Fingerprint, &l.ResultID, &l.UID, &l.ActorUID, &l.Invocation,
		&l.Decision, &l.Receipt, &l.Output, &l.Deadline, &l.DrainUntil, &l.CleanupAt,
		&l.ResumeSent, &l.Truncated, &l.Suspended, &l.Clean, &l.ExitCode)
}

// Register persists intent only once the A2A task is visible and open.
func (s *Store) Register(ctx context.Context, l Launch) (bool, error) {
	return s.change(ctx, `INSERT INTO ax_launches (task_id, task_name, fingerprint, result_id, deadline)
		SELECT ?, ?, ?, ?, ? FROM a2a_tasks WHERE id = ? AND state IN ('TASK_STATE_SUBMITTED', 'TASK_STATE_WORKING')
		ON CONFLICT(task_id) DO NOTHING`, l.TaskID, l.Name, l.Fingerprint, l.ResultID, l.Deadline, l.TaskID)
}

// Launch reads an association by A2A task ID.
func (s *Store) Launch(ctx context.Context, id string) (Launch, error) {
	return readLaunch(ctx, s.Reader(), "task_id", id)
}

// LaunchByName reads an association by its scoped AX Task name.
func (s *Store) LaunchByName(ctx context.Context, name string) (Launch, error) {
	return readLaunch(ctx, s.Reader(), "task_name", name)
}

func readLaunch(ctx context.Context, q kit.Querier, key, value string) (Launch, error) {
	var l Launch
	err := l.scan(q.QueryRow(ctx, `SELECT `+launchColumns+` FROM ax_launches WHERE `+key+` = ?`, value).Scan)
	return l, err
}

func (s *Store) change(ctx context.Context, stmt string, args ...any) (bool, error) {
	var count int64
	err := s.Write(ctx, func(q kit.Querier) error {
		var err error
		count, err = q.Exec(ctx, stmt, args...)
		return err
	})
	return count > 0 && err == nil, err
}

// Dispatch leaves a durable reconciliation window before issuing an AX mutation.
// Only an open launch can dispatch. Once claimed, further Resume calls stop.
func (s *Store) Dispatch(ctx context.Context, id string, resume bool, drainUntil time.Time) (bool, error) {
	if resume {
		return s.change(ctx, `UPDATE ax_launches SET resume_sent = 1, drain_until = ?
			WHERE task_id = ? AND decision = '' AND deadline > ? AND task_uid <> '' AND invocation_id = ''`,
			drainUntil.UnixNano(), id, s.now().UnixNano())
	}
	return s.change(ctx, `UPDATE ax_launches SET drain_until = ? WHERE task_id = ? AND decision = '' AND deadline > ?`,
		drainUntil.UnixNano(), id, s.now().UnixNano())
}

// Bind records an observed Task UID and, when known, its original actor UID.
func (s *Store) Bind(ctx context.Context, id, uid, actor string) (bool, error) {
	return s.change(ctx, `UPDATE ax_launches SET task_uid = ?, actor_uid = CASE WHEN ? = '' THEN actor_uid ELSE ? END
		WHERE task_id = ? AND (task_uid = '' OR task_uid = ?) AND (? = '' OR actor_uid = '' OR actor_uid = ?)`,
		uid, actor, actor, id, uid, actor, actor)
}

// Drained clears the reconciliation window after a definitive AX response.
func (s *Store) Drained(ctx context.Context, id string) error {
	_, err := s.change(ctx, `UPDATE ax_launches SET drain_until = 0 WHERE task_id = ?`, id)
	return err
}

// Decide records a timeout or confirmed failure if that decision can still win.
func (s *Store) Decide(ctx context.Context, id, decision string) (bool, error) {
	if decision == TimedOut {
		return s.change(ctx, `UPDATE ax_launches SET decision = ? WHERE task_id = ? AND decision = '' AND deadline <= ?`,
			decision, id, s.now().UnixNano())
	}
	return s.change(ctx, `UPDATE ax_launches SET decision = ? WHERE task_id = ? AND decision = '' AND deadline > ?`, decision, id, s.now().UnixNano())
}

// RequestCancel closes a live launch or records cancellation before intent.
func (s *Store) RequestCancel(ctx context.Context, id string) (bool, error) {
	var accepted bool
	err := s.Write(ctx, func(q kit.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO ax_launches (task_id, task_name, fingerprint, result_id, deadline, decision, clean)
			VALUES (?, '', '', ?, ?, ?, ?) ON CONFLICT(task_id) DO NOTHING`, id, "cancel:"+id, s.now().UnixNano(), Canceled, CleanDone)
		if err != nil {
			return err
		}
		changed, err := q.Exec(ctx, `UPDATE ax_launches SET decision = ?
			WHERE task_id = ? AND (decision = ? OR (decision = '' AND deadline > ?))`,
			Canceled, id, Canceled, s.now().UnixNano())
		accepted = changed != 0
		return err
	})
	return accepted && err == nil, err
}

// Updating the key to itself locks just this receipt through commit. The kit's
// Postgres fence holds a shared lock, so it cannot serialize same-turn writers.
func lockReceipt(ctx context.Context, q kit.Querier, id string) (Launch, error) {
	n, err := q.Exec(ctx, `UPDATE ax_launches SET resume_sent = resume_sent WHERE result_id = ?`, id)
	if err != nil {
		return Launch{}, err
	}
	if n == 0 {
		return Launch{}, kit.ErrNoRows
	}
	return readLaunch(ctx, q, "result_id", id)
}

// Claim commits one invocation before the wrapper can start its command.
func (s *Store) Claim(ctx context.Context, c result.Claim) (int, error) {
	status := http.StatusNotFound
	err := s.Write(ctx, func(q kit.Querier) error {
		l, err := lockReceipt(ctx, q, c.ResultID)
		if errors.Is(err, kit.ErrNoRows) {
			return nil
		}
		if err != nil || c.TaskUID != l.UID || l.ResumeSent == 0 {
			return err
		}
		switch {
		case l.Decision != "" || l.CleanupAt != 0 || s.now().UnixNano() >= l.Deadline:
			status = http.StatusGone
		case l.Invocation == c.InvocationID:
			status = http.StatusNoContent
		case l.Invocation != "":
			status = http.StatusConflict
		default:
			_, err = q.Exec(ctx, `UPDATE ax_launches SET invocation_id = ? WHERE task_id = ?`, c.InvocationID, l.TaskID)
			status = http.StatusNoContent
		}
		return err
	})
	return status, err
}

// AcceptResult commits a first result and returns its launch. Receipt replays
// return the protocol status without a newly accepted launch.
func (s *Store) AcceptResult(ctx context.Context, r result.Report, hash [32]byte) (int, *Launch, error) {
	var accepted *Launch
	digest := hex.EncodeToString(hash[:])
	status := http.StatusNotFound
	err := s.Write(ctx, func(q kit.Querier) error {
		l, err := lockReceipt(ctx, q, r.ResultID)
		if errors.Is(err, kit.ErrNoRows) {
			return nil
		}
		if err != nil || r.TaskUID != l.UID || r.InvocationID != l.Invocation {
			return err
		}
		switch {
		case l.Receipt == digest:
			status = http.StatusNoContent
		case l.Receipt != "":
			status = http.StatusConflict
		case l.Decision != "" || l.CleanupAt != 0 || s.now().UnixNano() >= l.Deadline:
			status = http.StatusGone
		default:
			decision, output := Succeeded, r.Output
			if r.ExitCode != 0 {
				decision, output = CommandFail, ""
			}
			truncated := 0
			if r.Truncated {
				truncated = 1
			}
			// PostgreSQL text cannot contain NUL, which is valid in the wire payload.
			output = hex.EncodeToString([]byte(output))
			_, err = q.Exec(ctx, `UPDATE ax_launches SET decision = ?, receipt = ?, exit_code = ?, output = ?, truncated = ?
				WHERE task_id = ?`, decision, digest, r.ExitCode, output, truncated, l.TaskID)
			status = http.StatusNoContent
			accepted = &l
		}
		return err
	})
	if err != nil {
		return status, nil, err
	}
	return status, accepted, nil
}

const cleanupSchedule = `CASE WHEN decision = ? AND EXISTS (SELECT 1 FROM a2a_tasks t
	WHERE t.id = ax_launches.task_id AND t.state = ?) THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) END`

// Recover closes unfinished launches and schedules cleanup for any terminal
// snapshot committed before a previous leader could schedule it.
func (s *Store) Recover(ctx context.Context, keep time.Duration) error {
	now := s.now()
	return s.Write(ctx, func(q kit.Querier) error {
		_, err := q.Exec(ctx, `UPDATE ax_launches
			SET decision = CASE WHEN EXISTS (SELECT 1 FROM a2a_tasks t WHERE t.id = ax_launches.task_id
			   AND t.state NOT IN (?, ?, ?)) THEN COALESCE(NULLIF(decision, ''), ?) ELSE ? END,
			    cleanup_at = `+cleanupSchedule+`,
			    output = ''
			WHERE clean = ? AND cleanup_at = 0`,
			string(a2a.TaskStateSubmitted), string(a2a.TaskStateWorking), string(a2a.TaskStateInputRequired), Failed, Interrupted,
			CommandFail, string(a2a.TaskStateFailed), now.Add(keep).UnixNano(), now.UnixNano(), CleanPending)
		return err
	})
}

// ScheduleCleanup sets the inspection deadline and drops the settled output.
func (s *Store) ScheduleCleanup(ctx context.Context, id string, at time.Time) error {
	_, err := s.change(ctx, `UPDATE ax_launches SET cleanup_at = ?, output = '' WHERE task_id = ? AND cleanup_at = 0`, at.UnixNano(), id)
	return err
}

// PendingCleanup lists scheduled AX Task cleanup.
func (s *Store) PendingCleanup(ctx context.Context) ([]Launch, error) {
	var launches []Launch
	err := s.Reader().Query(ctx, `SELECT `+launchColumns+` FROM ax_launches
		WHERE clean = ? AND cleanup_at <> 0 ORDER BY cleanup_at, task_id`, []any{CleanPending}, func(scan func(...any) error) error {
		var l Launch
		if err := l.scan(scan); err != nil {
			return err
		}
		launches = append(launches, l)
		return nil
	})
	return launches, err
}

// MarkClean records that AX confirmed the Task is gone.
func (s *Store) MarkClean(ctx context.Context, id string) error {
	_, err := s.change(ctx, `UPDATE ax_launches SET clean = ? WHERE task_id = ?`, CleanDone, id)
	return err
}

// MarkSuspended records that AX acknowledged suspension.
func (s *Store) MarkSuspended(ctx context.Context, id string) error {
	_, err := s.change(ctx, `UPDATE ax_launches SET suspended = 1 WHERE task_id = ?`, id)
	return err
}

// PruneReceipts removes expired receipts with confirmed cleanup.
func (s *Store) PruneReceipts(ctx context.Context, before time.Time) error {
	_, err := s.change(ctx, `DELETE FROM ax_launches WHERE clean = ? AND deadline < ? AND cleanup_at < ?`,
		CleanDone, before.UnixNano(), before.UnixNano())
	return err
}

// ScheduleSettled recovers a cleanup schedule if the SDK committed a terminal
// snapshot but its cleanup hook could not record the schedule.
func (s *Store) ScheduleSettled(ctx context.Context, keep time.Duration) error {
	const eligible = `clean = ? AND cleanup_at = 0 AND EXISTS (SELECT 1 FROM a2a_tasks t
		WHERE t.id = ax_launches.task_id AND t.state IN (?, ?, ?, ?))`
	args := []any{CleanPending, string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed),
		string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected)}
	var pending bool
	err := s.Reader().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ax_launches WHERE `+eligible+`)`, args...).Scan(&pending)
	if err != nil || !pending {
		return err
	}
	now := s.now()
	_, err = s.change(ctx, `UPDATE ax_launches SET output = '',
		decision = CASE WHEN decision = '' THEN ? ELSE decision END,
		cleanup_at = `+cleanupSchedule+` WHERE `+eligible,
		append([]any{Failed, CommandFail, string(a2a.TaskStateFailed), now.Add(keep).UnixNano(), now.UnixNano()}, args...)...)
	return err
}

// MarkForeign preserves the association so an ownership mismatch cannot later
// be mistaken for a Task that arrived without a launch record.
func (s *Store) MarkForeign(ctx context.Context, id string) error {
	_, err := s.change(ctx, `UPDATE ax_launches SET clean = ? WHERE task_id = ?`, CleanForeign, id)
	return err
}

// CheckFence verifies this replica still holds the lease before an external
// cleanup operation. An AX request already in flight may outlive the lease.
func (s *Store) CheckFence(ctx context.Context) error {
	return s.Write(ctx, func(kit.Querier) error { return nil })
}
