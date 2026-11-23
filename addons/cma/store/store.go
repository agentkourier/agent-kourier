// Package store keeps the adapter's state where a restart finds it: the A2A tasks, the message IDs that started or
// joined them, and each A2A context's session. It runs on SQLite (one file, one adapter) or Postgres (shared by
// replicas). Both run the same SQL: times are Unix nanoseconds in 64-bit integers, which keeps page tokens exact and
// the SQL the same on both, and the Postgres driver rewrites the ? placeholders.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// Session is an A2A context's CMA session.
type Session struct {
	ContextID string
	SessionID string
	// LastActive is when a turn of the context last started or ended.
	LastActive time.Time
}

// Options configures a store.
type Options struct {
	// Now stamps each task's update time. Nil is time.Now.
	Now func() time.Time
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write finds that another replica has taken the lease since this store took
	// it, before the write fails with kitstore.ErrDeposed. It runs in the writer's goroutine and must not call the
	// store. The adapter ends its process in it: the error alone would reach whichever caller the write was made for.
	// It does not run for a store that holds no epoch (a standby's, or one that released its lease).
	OnDeposed func()
}

// Store is the adapter's state on SQLite or Postgres: its tables on the kit's database, whose lease (TryLead, Renew,
// Release, CurrentLeader) fences every write of the store. It implements taskstore.Store. Tasks are not partitioned by
// owner: every caller that passes the caller check is Agent Kourier, so the owner is recorded and nothing more.
type Store struct {
	*kitstore.DB
	now func() time.Time
	log *slog.Logger
}

var _ taskstore.Store = (*Store)(nil)

func newStore(db *kitstore.DB, opts Options) *Store {
	s := &Store{DB: db, now: opts.Now, log: opts.Log}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// owner is the authenticated user of the call, or "" when the call has none.
func owner(ctx context.Context) string {
	if cc, ok := a2asrv.CallContextFrom(ctx); ok && cc.User != nil && cc.User.Authenticated {
		return cc.User.Name
	}
	return ""
}

func nanos(t time.Time) int64     { return t.UnixNano() }
func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }
func statusAt(task *a2a.Task) *int64 {
	if task.Status.Timestamp == nil {
		return nil
	}
	n := nanos(*task.Status.Timestamp)
	return &n
}

func encode(task *a2a.Task) ([]byte, error) {
	if task == nil || task.ID == "" {
		return nil, fmt.Errorf("%w: a task needs an ID", a2a.ErrInvalidParams)
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("encode task %s: %w", task.ID, err)
	}
	return raw, nil
}

func decode(raw []byte) (*a2a.Task, error) {
	task := &a2a.Task{}
	if err := json.Unmarshal(raw, task); err != nil {
		return nil, fmt.Errorf("decode task: %w", err)
	}
	return task, nil
}

// Create implements taskstore.Store. It records the call's user as the task's owner.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	raw, err := encode(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	err = s.Write(ctx, func(q kitstore.Querier) error {
		n, err := q.Exec(ctx, `INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			string(task.ID), task.ContextID, owner(ctx), string(task.Status.State), statusAt(task), nanos(s.now()), raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return taskstore.ErrTaskAlreadyExists
		}
		return nil
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("create task %s: %w", task.ID, err)
	}
	return 1, nil
}

// Update implements taskstore.Store. The owner stays the creator's.
func (s *Store) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: no update", a2a.ErrInvalidParams)
	}
	raw, err := encode(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	task := req.Task
	var version int64
	err = s.Write(ctx, func(q kitstore.Querier) error {
		query := `UPDATE a2a_tasks SET context_id = ?, state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
			WHERE id = ?`
		args := []any{task.ContextID, string(task.Status.State), statusAt(task), nanos(s.now()), raw, string(task.ID)}
		if req.PrevVersion != taskstore.TaskVersionMissing {
			query += ` AND version = ?`
			args = append(args, int64(req.PrevVersion))
		}
		err := q.QueryRow(ctx, query+` RETURNING version`, args...).Scan(&version)
		if !errors.Is(err, kitstore.ErrNoRows) {
			return err
		}
		var one int
		switch err := q.QueryRow(ctx, `SELECT 1 FROM a2a_tasks WHERE id = ?`, string(task.ID)).Scan(&one); {
		case errors.Is(err, kitstore.ErrNoRows):
			return a2a.ErrTaskNotFound
		case err != nil:
			return err
		}
		return taskstore.ErrConcurrentModification
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update task %s: %w", task.ID, err)
	}
	return taskstore.TaskVersion(version), nil
}

// Get implements taskstore.Store.
func (s *Store) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	var raw []byte
	var version int64
	var user string
	err := s.Reader().QueryRow(ctx, `SELECT payload, version, owner FROM a2a_tasks WHERE id = ?`, string(id)).Scan(&raw, &version, &user)
	if errors.Is(err, kitstore.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	task, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: user}, nil
}

// The list's page sizes and the history it keeps by default, as a2a-go's in-memory store has them.
const (
	defaultPageSize      = 50
	maxPageSize          = 100
	defaultHistoryLength = 100
)

// List implements taskstore.Store: the tasks that match the request's context, state and status time (a task whose
// status has no time matches any), newest update first, in pages.
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req == nil {
		req = &a2a.ListTasksRequest{}
	}
	size := req.PageSize
	if size == 0 {
		size = defaultPageSize
	}
	if size < 1 || size > maxPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d: %w", maxPageSize, req.PageSize, a2a.ErrInvalidRequest)
	}
	var where []string
	var args []any
	if req.ContextID != "" {
		where, args = append(where, `context_id = ?`), append(args, req.ContextID)
	}
	if req.Status != a2a.TaskStateUnspecified {
		where, args = append(where, `state = ?`), append(args, string(req.Status))
	}
	if req.StatusTimestampAfter != nil {
		where, args = append(where, `(status_at IS NULL OR status_at >= ?)`), append(args, nanos(*req.StatusTimestampAfter))
	}
	q := s.Reader()
	var total int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM a2a_tasks`+whereClause(where), args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count tasks: %w", err)
	}
	if req.PageToken != "" {
		at, id, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		where, args = append(where, `(updated_at < ? OR (updated_at = ? AND id < ?))`), append(args, at, at, id)
	}
	type row struct {
		raw []byte
		at  int64
		id  string
	}
	var rows []row
	err := q.Query(ctx, `SELECT payload, updated_at, id FROM a2a_tasks`+whereClause(where)+` ORDER BY updated_at DESC, id DESC LIMIT ?`,
		append(args, size+1), func(scan func(...any) error) error {
			var r row
			if err := scan(&r.raw, &r.at, &r.id); err != nil {
				return err
			}
			rows = append(rows, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, TotalSize: total, PageSize: size}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		resp.NextPageToken = encodePageToken(last.at, last.id)
	}
	history := defaultHistoryLength
	if req.HistoryLength != nil {
		history = *req.HistoryLength
	}
	for _, r := range rows {
		task, err := decode(r.raw)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		if history <= 0 {
			task.History = []*a2a.Message{}
		} else if len(task.History) > history {
			task.History = task.History[len(task.History)-history:]
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		resp.Tasks = append(resp.Tasks, task)
	}
	return resp, nil
}

func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return ` WHERE ` + strings.Join(where, ` AND `)
}

// TasksInStates returns the tasks whose state is one of states.
func (s *Store) TasksInStates(ctx context.Context, states ...a2a.TaskState) ([]*taskstore.StoredTask, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(states))
	for _, st := range states {
		args = append(args, string(st))
	}
	var out []*taskstore.StoredTask
	err := s.Reader().Query(ctx, `SELECT payload, version, owner FROM a2a_tasks WHERE state IN (`+marks(len(states))+`) ORDER BY id`,
		args, func(scan func(...any) error) error {
			var raw []byte
			var version int64
			var user string
			if err := scan(&raw, &version, &user); err != nil {
				return err
			}
			task, err := decode(raw)
			if err != nil {
				return err
			}
			out = append(out, &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: user})
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list tasks in states %v: %w", states, err)
	}
	return out, nil
}

// settled are the terminal states: a task in one can never change again.
var settled = []any{string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected)}

// DeleteSettledBefore deletes the settled tasks last updated before cutoff, and their message IDs, and returns how
// many tasks it deleted.
func (s *Store) DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	filter := `state IN (` + marks(len(settled)) + `) AND updated_at < ?`
	args := append(append([]any{}, settled...), nanos(cutoff))
	err := s.Write(ctx, func(q kitstore.Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM message_ids WHERE task_id IN (SELECT id FROM a2a_tasks WHERE `+filter+`)`, args...); err != nil {
			return err
		}
		var err error
		n, err = q.Exec(ctx, `DELETE FROM a2a_tasks WHERE `+filter, args...)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete settled tasks: %w", err)
	}
	return n, nil
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// MessageTask returns the task that messageID started or joined; ok is false when the message is not recorded.
func (s *Store) MessageTask(ctx context.Context, messageID string) (id a2a.TaskID, ok bool, err error) {
	var task string
	err = s.Reader().QueryRow(ctx, `SELECT task_id FROM message_ids WHERE message_id = ?`, messageID).Scan(&task)
	if errors.Is(err, kitstore.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up message %s: %w", messageID, err)
	}
	return a2a.TaskID(task), true, nil
}

// RecordMessage records that messageID started or joined taskID. A message ID already recorded keeps its task.
func (s *Store) RecordMessage(ctx context.Context, messageID string, taskID a2a.TaskID) error {
	err := s.Write(ctx, func(q kitstore.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO message_ids (message_id, task_id, created_at) VALUES (?, ?, ?) ON CONFLICT (message_id) DO NOTHING`,
			messageID, string(taskID), nanos(s.now()))
		return err
	})
	if err != nil {
		return fmt.Errorf("record message %s: %w", messageID, err)
	}
	return nil
}

// Sessions returns every context's session.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	var out []Session
	err := s.Reader().Query(ctx, `SELECT context_id, session_id, last_active FROM context_sessions ORDER BY context_id`, nil,
		func(scan func(...any) error) error {
			var ss Session
			var at int64
			if err := scan(&ss.ContextID, &ss.SessionID, &at); err != nil {
				return err
			}
			ss.LastActive = fromNanos(at)
			out = append(out, ss)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// PutSession records the context's session, replacing the one it had.
func (s *Store) PutSession(ctx context.Context, ss Session) error {
	err := s.Write(ctx, func(q kitstore.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO context_sessions (context_id, session_id, last_active) VALUES (?, ?, ?)
			ON CONFLICT (context_id) DO UPDATE SET session_id = excluded.session_id, last_active = excluded.last_active`,
			ss.ContextID, ss.SessionID, nanos(ss.LastActive))
		return err
	})
	if err != nil {
		return fmt.Errorf("put the session of context %s: %w", ss.ContextID, err)
	}
	return nil
}

// TouchSession sets the last activity of the context's session, while that session is still sessionID.
func (s *Store) TouchSession(ctx context.Context, contextID, sessionID string, at time.Time) error {
	err := s.Write(ctx, func(q kitstore.Querier) error {
		_, err := q.Exec(ctx, `UPDATE context_sessions SET last_active = ? WHERE context_id = ? AND session_id = ?`, nanos(at), contextID, sessionID)
		return err
	})
	if err != nil {
		return fmt.Errorf("touch the session of context %s: %w", contextID, err)
	}
	return nil
}

// ForgetContext is for a context whose session, sessionID, was archived. It removes that session, if the context
// still has it, and deletes the context's tasks last updated before cutoff, with their message IDs: they belong to the
// archived session and can never run again. A task the context started after cutoff, in a new session, is kept.
func (s *Store) ForgetContext(ctx context.Context, contextID, sessionID string, cutoff time.Time) error {
	err := s.Write(ctx, func(q kitstore.Querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM context_sessions WHERE context_id = ? AND session_id = ?`, contextID, sessionID); err != nil {
			return err
		}
		filter := `context_id = ? AND updated_at < ?`
		if _, err := q.Exec(ctx, `DELETE FROM message_ids WHERE task_id IN (SELECT id FROM a2a_tasks WHERE `+filter+`)`, contextID, nanos(cutoff)); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `DELETE FROM a2a_tasks WHERE `+filter, contextID, nanos(cutoff))
		return err
	})
	if err != nil {
		return fmt.Errorf("forget context %s: %w", contextID, err)
	}
	return nil
}
