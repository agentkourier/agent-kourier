// Package store keeps the agent's state where a restart finds it: the A2A tasks, the message IDs that started or
// joined them, when each A2A context was last active, and ADK's sessions and their events, which ADK writes through
// gorm on the same database. It runs on SQLite (one file, one agent) or Postgres (shared by replicas). Both run the
// same SQL: times are Unix nanoseconds in 64-bit integers, which keeps page tokens exact and the SQL the same on both,
// and the Postgres querier rewrites the ? placeholders.
//
// Tasks and message IDs belong to the caller that created them (package auth): every read and write made for an A2A
// call is limited to the call's caller, and one without an authenticated caller is refused. The upkeep (SettleInterrupted,
// DeleteSettledBefore, DeleteIdleContexts) works across callers.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/adk/v2/session"
)

// AppName is the ADK app the agent runs its sessions under; the store deletes an idle context's session of it.
const AppName = "agent-kourier-ax"

// InterruptedMessage is the agent message of a task that a restart failed.
const InterruptedMessage = "Interrupted by an agent restart."

// Options configures a store.
type Options struct {
	// Now stamps each task's update time and each context's activity. Nil is time.Now.
	Now func() time.Time
	// Log receives what the store finds worth a warning. Nil is slog.Default().
	Log *slog.Logger
}

// Store is the agent's state on SQLite or Postgres. It implements taskstore.Store, and its Sessions is ADK's session
// service on the same database.
type Store struct {
	conn conn
	now  func() time.Time
	log  *slog.Logger

	// fence, when set, runs first in every write transaction, the store's (write) and ADK's (a gorm callback, adk.go),
	// and its error aborts the write. It is the one place a check that must hold for any write runs: a leader's lease
	// epoch, on Postgres, where several replicas share the store.
	fence func(ctx context.Context, q querier) error

	sessions session.Service

	closeOnce sync.Once
	closeErr  error
}

var _ taskstore.Store = (*Store)(nil)

func newStore(c conn, opts Options) *Store {
	s := &Store{conn: c, now: opts.Now, log: opts.Log}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Sessions is ADK's session service, on the store's database.
func (s *Store) Sessions() session.Service { return s.sessions }

// Close releases the store. Closing it again does nothing.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.conn.close() })
	return s.closeErr
}

// write runs fn in one write transaction, after the fence. Every write of the store goes through it.
func (s *Store) write(ctx context.Context, fn func(q querier) error) error {
	return s.conn.inTx(ctx, func(q querier) error {
		if s.fence != nil {
			if err := s.fence(ctx, q); err != nil {
				return err
			}
		}
		return fn(q)
	})
}

// owner is the authenticated caller of the call. A call without one is refused.
func owner(ctx context.Context) (string, error) {
	if cc, ok := a2asrv.CallContextFrom(ctx); ok && cc.User != nil && cc.User.Authenticated && cc.User.Name != "" {
		return cc.User.Name, nil
	}
	return "", a2a.ErrUnauthenticated
}

func nanos(t time.Time) int64 { return t.UnixNano() }

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

// Create implements taskstore.Store. It records the call's caller as the task's owner, the IDs of the task's user
// messages, and the context's activity, in one transaction.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	user, err := owner(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	raw, err := encode(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	now := nanos(s.now())
	err = s.write(ctx, func(q querier) error {
		n, err := q.exec(ctx, `INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			string(task.ID), task.ContextID, user, string(task.Status.State), statusAt(task), now, raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return taskstore.ErrTaskAlreadyExists
		}
		return recordActivity(ctx, q, user, task, now)
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("create task %s: %w", task.ID, err)
	}
	return 1, nil
}

// Update implements taskstore.Store, for a task of the call's caller. The message IDs it records stay recorded.
func (s *Store) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	user, err := owner(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if req == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: no update", a2a.ErrInvalidParams)
	}
	raw, err := encode(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	task := req.Task
	now := nanos(s.now())
	var version int64
	err = s.write(ctx, func(q querier) error {
		query := `UPDATE a2a_tasks SET context_id = ?, state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
			WHERE id = ? AND owner = ?`
		args := []any{task.ContextID, string(task.Status.State), statusAt(task), now, raw, string(task.ID), user}
		if req.PrevVersion != taskstore.TaskVersionMissing {
			query += ` AND version = ?`
			args = append(args, int64(req.PrevVersion))
		}
		err := q.queryRow(ctx, query+` RETURNING version`, args...).Scan(&version)
		if errors.Is(err, errNoRows) {
			var one int
			switch err := q.queryRow(ctx, `SELECT 1 FROM a2a_tasks WHERE id = ? AND owner = ?`, string(task.ID), user).Scan(&one); {
			case errors.Is(err, errNoRows):
				return a2a.ErrTaskNotFound
			case err != nil:
				return err
			}
			return taskstore.ErrConcurrentModification
		}
		if err != nil {
			return err
		}
		return recordActivity(ctx, q, user, task, now)
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update task %s: %w", task.ID, err)
	}
	return taskstore.TaskVersion(version), nil
}

// recordActivity records the IDs of the task's user messages, refusing one that another task of the owner holds, and
// the time as its context's last activity.
func recordActivity(ctx context.Context, q querier, user string, task *a2a.Task, now int64) error {
	for _, msg := range task.History {
		if msg == nil || msg.Role != a2a.MessageRoleUser || msg.ID == "" {
			continue
		}
		n, err := q.exec(ctx, `INSERT INTO message_ids (owner, message_id, task_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (owner, message_id) DO NOTHING`, user, msg.ID, string(task.ID), now)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		var held string
		if err := q.queryRow(ctx, `SELECT task_id FROM message_ids WHERE owner = ? AND message_id = ?`, user, msg.ID).Scan(&held); err != nil {
			return err
		}
		if held != string(task.ID) {
			return fmt.Errorf("message %s belongs to task %s: %w", msg.ID, held, taskstore.ErrTaskAlreadyExists)
		}
	}
	if task.ContextID == "" {
		return nil
	}
	return touch(ctx, q, user, task.ContextID, now)
}

// touch records now as the owner's context's last activity.
func touch(ctx context.Context, q querier, user, contextID string, now int64) error {
	_, err := q.exec(ctx, `INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)
		ON CONFLICT (owner, context_id) DO UPDATE SET last_active = excluded.last_active`, user, contextID, now)
	return err
}

// TouchContext records now as the last activity of the call's caller's context, in a transaction of its own. The
// agent calls it when a message for the context arrives, before the turn reads the context's session, so that
// DeleteIdleContexts, which deletes a context only if it is still idle in its own transaction, keeps it.
func (s *Store) TouchContext(ctx context.Context, contextID string) error {
	user, err := owner(ctx)
	if err != nil {
		return err
	}
	if err := s.write(ctx, func(q querier) error { return touch(ctx, q, user, contextID, nanos(s.now())) }); err != nil {
		return fmt.Errorf("touch context %s: %w", contextID, err)
	}
	return nil
}

// Get implements taskstore.Store: a task of the call's caller.
func (s *Store) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var raw []byte
	var version int64
	err = s.conn.reader().queryRow(ctx, `SELECT payload, version FROM a2a_tasks WHERE id = ? AND owner = ?`, string(id), user).Scan(&raw, &version)
	if errors.Is(err, errNoRows) {
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

// MessageTask returns the task of the call's caller that messageID started or joined, or nil when the message is not
// recorded.
func (s *Store) MessageTask(ctx context.Context, messageID string) (*a2a.Task, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var id string
	err = s.conn.reader().queryRow(ctx, `SELECT task_id FROM message_ids WHERE owner = ? AND message_id = ?`, user, messageID).Scan(&id)
	if errors.Is(err, errNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up message %s: %w", messageID, err)
	}
	stored, err := s.Get(ctx, a2a.TaskID(id))
	if err != nil {
		return nil, fmt.Errorf("look up message %s: %w", messageID, err)
	}
	return stored.Task, nil
}

// The list's page sizes and the history it keeps by default, as a2a-go's in-memory store has them.
const (
	defaultPageSize      = 50
	maxPageSize          = 100
	defaultHistoryLength = 100
)

// List implements taskstore.Store: the call's caller's tasks that match the request's context, state and status time
// (a task whose status has no time matches any), newest update first, in pages.
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
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
	if req.HistoryLength != nil && *req.HistoryLength < 0 {
		return nil, fmt.Errorf("history length must not be negative: %w", a2a.ErrInvalidParams)
	}
	where, args := []string{`owner = ?`}, []any{user}
	if req.ContextID != "" {
		where, args = append(where, `context_id = ?`), append(args, req.ContextID)
	}
	if req.Status != a2a.TaskStateUnspecified {
		where, args = append(where, `state = ?`), append(args, string(req.Status))
	}
	if req.StatusTimestampAfter != nil {
		where, args = append(where, `(status_at IS NULL OR status_at >= ?)`), append(args, nanos(*req.StatusTimestampAfter))
	}
	q := s.conn.reader()
	var total int
	if err := q.queryRow(ctx, `SELECT COUNT(*) FROM a2a_tasks WHERE `+strings.Join(where, ` AND `), args...).Scan(&total); err != nil {
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
	err = q.query(ctx, `SELECT payload, updated_at, id FROM a2a_tasks WHERE `+strings.Join(where, ` AND `)+` ORDER BY updated_at DESC, id DESC LIMIT ?`,
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

// running are the states of a task whose turn is under way; settled, the terminal ones, in which a task never changes
// again.
var (
	running = []any{string(a2a.TaskStateSubmitted), string(a2a.TaskStateWorking)}
	settled = []any{string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected)}
)

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// SettleInterrupted fails every task a restart left submitted or working, with the agent message InterruptedMessage,
// and returns how many it failed. A task paused on a question (input-required) survives, and its answer resumes it.
// It fails every such task in the store, whichever process ran it, so call it only when no turn is running anywhere on
// the database: before serving, and on Postgres only where no other replica serves turns from it.
func (s *Store) SettleInterrupted(ctx context.Context) (int, error) {
	n := 0
	err := s.write(ctx, func(q querier) error {
		type interrupted struct {
			id      string
			version int64
			task    *a2a.Task
		}
		var found []interrupted
		err := q.query(ctx, `SELECT id, version, payload FROM a2a_tasks WHERE state IN (`+marks(len(running))+`) ORDER BY id`, running,
			func(scan func(...any) error) error {
				var it interrupted
				var raw []byte
				if err := scan(&it.id, &it.version, &raw); err != nil {
					return err
				}
				task, err := decode(raw)
				if err != nil {
					return fmt.Errorf("task %s: %w", it.id, err)
				}
				it.task = task
				found = append(found, it)
				return nil
			})
		if err != nil {
			return err
		}
		for _, it := range found {
			now := s.now()
			it.task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed, Timestamp: &now,
				Message: a2a.NewMessageForTask(a2a.MessageRoleAgent, it.task, a2a.NewTextPart(InterruptedMessage))}
			raw, err := encode(it.task)
			if err != nil {
				return err
			}
			if _, err := q.exec(ctx, `UPDATE a2a_tasks SET state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
				WHERE id = ? AND version = ?`, string(a2a.TaskStateFailed), nanos(now), nanos(now), raw, it.id, it.version); err != nil {
				return fmt.Errorf("task %s: %w", it.id, err)
			}
		}
		n = len(found)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("fail the interrupted tasks: %w", err)
	}
	return n, nil
}

// DeleteSettledBefore deletes the settled tasks last updated before cutoff, and their message IDs, and returns how
// many tasks it deleted.
func (s *Store) DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	filter := `state IN (` + marks(len(settled)) + `) AND updated_at < ?`
	args := append(append([]any{}, settled...), nanos(cutoff))
	err := s.write(ctx, func(q querier) error {
		if _, err := q.exec(ctx, `DELETE FROM message_ids WHERE task_id IN (SELECT id FROM a2a_tasks WHERE `+filter+`)`, args...); err != nil {
			return err
		}
		var err error
		n, err = q.exec(ctx, `DELETE FROM a2a_tasks WHERE `+filter, args...)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete settled tasks: %w", err)
	}
	return n, nil
}

// DeleteIdleContexts deletes each context last active before cutoff, with no turn running: its ADK session and the
// session's events, its tasks (a question it paused on included) and their message IDs. The context's next message
// starts a new session. It returns how many contexts it deleted. Each context goes in a transaction of its own, which
// checks again that the context is still idle.
func (s *Store) DeleteIdleContexts(ctx context.Context, cutoff time.Time) (int, error) {
	type idle struct{ owner, contextID string }
	var found []idle
	err := s.conn.reader().query(ctx, `SELECT owner, context_id FROM contexts WHERE last_active < ? ORDER BY last_active`,
		[]any{nanos(cutoff)}, func(scan func(...any) error) error {
			var c idle
			if err := scan(&c.owner, &c.contextID); err != nil {
				return err
			}
			found = append(found, c)
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("find idle contexts: %w", err)
	}
	deleted := 0
	for _, c := range found {
		gone := false
		err := s.write(ctx, func(q querier) error {
			n, err := q.exec(ctx, `DELETE FROM contexts WHERE owner = ? AND context_id = ? AND last_active < ?
				AND NOT EXISTS (SELECT 1 FROM a2a_tasks WHERE owner = ? AND context_id = ? AND state IN (`+marks(len(running))+`))`,
				append([]any{c.owner, c.contextID, nanos(cutoff), c.owner, c.contextID}, running...)...)
			if err != nil || n == 0 {
				return err // n == 0: active again, or a turn is running
			}
			for _, stmt := range []string{
				`DELETE FROM events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
				`DELETE FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?`,
			} {
				if _, err := q.exec(ctx, stmt, AppName, c.owner, c.contextID); err != nil {
					return err
				}
			}
			if _, err := q.exec(ctx, `DELETE FROM message_ids WHERE owner = ? AND task_id IN (SELECT id FROM a2a_tasks WHERE owner = ? AND context_id = ?)`,
				c.owner, c.owner, c.contextID); err != nil {
				return err
			}
			if _, err := q.exec(ctx, `DELETE FROM a2a_tasks WHERE owner = ? AND context_id = ?`, c.owner, c.contextID); err != nil {
				return err
			}
			gone = true
			return nil
		})
		if err != nil {
			return deleted, fmt.Errorf("delete idle context %s: %w", c.contextID, err)
		}
		if gone {
			deleted++
		}
	}
	return deleted, nil
}
