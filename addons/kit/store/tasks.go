package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// The list's page sizes and the history it keeps by default, as a2a-go's in-memory store has them.
const (
	// DefaultPageSize is the page size of a List that names none.
	DefaultPageSize = 50
	// MaxPageSize is the largest page a List may ask for.
	MaxPageSize = 100
	// DefaultHistoryLength is how many of each task's latest messages a List returns when it names no history length.
	DefaultHistoryLength = 100
)

// TaskOptions configures a Store.
type TaskOptions struct {
	// Now stamps each task's update time and each context's activity. Nil is time.Now.
	Now func() time.Time
	// ContextActivity makes Create and Update record the task's context as active now, in the task's transaction: the
	// add-on's idle contexts are then the ones whose tasks have not changed for a while (DeleteIdleContexts). Off, a
	// context's activity is only what TouchContext, PutContext and TouchContextRef record.
	ContextActivity bool
	// ReleaseContext, if set, runs in the transaction in which DeleteIdleContexts deletes a context, after the
	// context's row and before its tasks: the add-on deletes there what it keeps of the context in the same database
	// (ADK's session). Its error rolls the deletion back.
	ReleaseContext func(ctx context.Context, q Querier, c Context) error
}

// Store is an add-on's A2A task state on its database: the tasks, the IDs of the user messages that started or joined
// them, and the contexts. It implements taskstore.Store, and every write runs the database's fence (Write).
//
// Tasks and message IDs belong to the caller that created them, the A2A call's authenticated user, which the add-on's
// caller check sets: every read and write made for an A2A call is limited to the call's caller, and one without an
// authenticated caller is refused with a2a.ErrUnauthenticated. The upkeep (SettleInterrupted, DeleteSettledBefore,
// DeleteIdleContexts) works across callers, and so do the context records an add-on names the owner of (Contexts,
// PutContext, TouchContextRef, ForgetContext).
type Store struct {
	*DB
	now             func() time.Time
	contextActivity bool
	releaseContext  func(ctx context.Context, q Querier, c Context) error
}

var _ taskstore.Store = (*Store)(nil)

// NewStore returns the task state on db, whose open applied the kit's migrations, which hold its tables.
func NewStore(db *DB, opts TaskOptions) *Store {
	s := &Store{DB: db, now: opts.Now, contextActivity: opts.ContextActivity, releaseContext: opts.ReleaseContext}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// owner is the authenticated caller of the call. A call without one is refused.
func owner(ctx context.Context) (string, error) {
	if cc, ok := a2asrv.CallContextFrom(ctx); ok && cc.User != nil && cc.User.Authenticated && cc.User.Name != "" {
		return cc.User.Name, nil
	}
	return "", a2a.ErrUnauthenticated
}

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

// Create implements taskstore.Store. It records the call's caller as the task's owner and the IDs of the task's user
// messages, and with ContextActivity the context's activity, in one transaction.
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
	err = s.Write(ctx, func(q Querier) error {
		n, err := q.Exec(ctx, `INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			string(task.ID), task.ContextID, user, string(task.Status.State), statusAt(task), now, raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return taskstore.ErrTaskAlreadyExists
		}
		return s.recordActivity(ctx, q, user, task, now)
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("create task %s: %w", task.ID, err)
	}
	return 1, nil
}

// Update implements taskstore.Store, for a task of the call's caller. The message IDs it records, in the update's
// transaction, stay recorded.
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
	err = s.Write(ctx, func(q Querier) error {
		query := `UPDATE a2a_tasks SET context_id = ?, state = ?, status_at = ?, updated_at = ?, payload = ?, version = version + 1
			WHERE id = ? AND owner = ?`
		args := []any{task.ContextID, string(task.Status.State), statusAt(task), now, raw, string(task.ID), user}
		if req.PrevVersion != taskstore.TaskVersionMissing {
			query += ` AND version = ?`
			args = append(args, int64(req.PrevVersion))
		}
		err := q.QueryRow(ctx, query+` RETURNING version`, args...).Scan(&version)
		if errors.Is(err, ErrNoRows) {
			var one int
			switch err := q.QueryRow(ctx, `SELECT 1 FROM a2a_tasks WHERE id = ? AND owner = ?`, string(task.ID), user).Scan(&one); {
			case errors.Is(err, ErrNoRows):
				return a2a.ErrTaskNotFound
			case err != nil:
				return err
			}
			return taskstore.ErrConcurrentModification
		}
		if err != nil {
			return err
		}
		return s.recordActivity(ctx, q, user, task, now)
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update task %s: %w", task.ID, err)
	}
	return taskstore.TaskVersion(version), nil
}

// recordActivity records the IDs of the task's user messages, refusing one that another task of the owner holds, and
// with ContextActivity the time as its context's last activity.
func (s *Store) recordActivity(ctx context.Context, q Querier, user string, task *a2a.Task, now int64) error {
	for _, msg := range task.History {
		if msg == nil || msg.Role != a2a.MessageRoleUser || msg.ID == "" {
			continue
		}
		n, err := q.Exec(ctx, `INSERT INTO message_ids (owner, message_id, task_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (owner, message_id) DO NOTHING`, user, msg.ID, string(task.ID), now)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		var held string
		if err := q.QueryRow(ctx, `SELECT task_id FROM message_ids WHERE owner = ? AND message_id = ?`, user, msg.ID).Scan(&held); err != nil {
			return err
		}
		if held != string(task.ID) {
			return fmt.Errorf("message %s belongs to task %s: %w", msg.ID, held, taskstore.ErrTaskAlreadyExists)
		}
	}
	if !s.contextActivity || task.ContextID == "" {
		return nil
	}
	return touch(ctx, q, user, task.ContextID, now)
}

// Get implements taskstore.Store: a task of the call's caller.
func (s *Store) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var raw []byte
	var version int64
	err = s.Reader().QueryRow(ctx, `SELECT payload, version FROM a2a_tasks WHERE id = ? AND owner = ?`, string(id), user).Scan(&raw, &version)
	if errors.Is(err, ErrNoRows) {
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
// recorded. Create and Update record a task's user messages in the task's own transaction, so a message whose task
// was stored is never missing here.
func (s *Store) MessageTask(ctx context.Context, messageID string) (*a2a.Task, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var id string
	err = s.Reader().QueryRow(ctx, `SELECT task_id FROM message_ids WHERE owner = ? AND message_id = ?`, user, messageID).Scan(&id)
	if errors.Is(err, ErrNoRows) {
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
		size = DefaultPageSize
	}
	if size < 1 || size > MaxPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d: %w", MaxPageSize, req.PageSize, a2a.ErrInvalidRequest)
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
	q := s.Reader()
	var total int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM a2a_tasks WHERE `+strings.Join(where, ` AND `), args...).Scan(&total); err != nil {
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
	err = q.Query(ctx, `SELECT payload, updated_at, id FROM a2a_tasks WHERE `+strings.Join(where, ` AND `)+` ORDER BY updated_at DESC, id DESC LIMIT ?`,
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
	history := DefaultHistoryLength
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

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }
