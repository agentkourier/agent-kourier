package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// TaskStore persists A2A snapshots independently of ADK's conversation events.
type TaskStore struct{ DB *sql.DB }

var _ taskstore.Store = (*TaskStore)(nil)

func owner(ctx context.Context) (string, error) {
	cc, ok := a2asrv.CallContextFrom(ctx)
	if !ok || cc.User == nil || !cc.User.Authenticated || cc.User.Name == "" {
		return "", a2a.ErrUnauthenticated
	}
	return cc.User.Name, nil
}

// Init creates the example's tables and settles work interrupted by a process restart.
func (s *TaskStore) Init(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS a2a_tasks(id TEXT PRIMARY KEY, owner TEXT NOT NULL, version INTEGER NOT NULL, payload BLOB NOT NULL); CREATE TABLE IF NOT EXISTS a2a_messages(owner TEXT NOT NULL, message_id TEXT NOT NULL, task_id TEXT NOT NULL, PRIMARY KEY(owner,message_id))`); err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,payload FROM a2a_tasks`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type changed struct {
		id      string
		payload []byte
	}
	var changes []changed
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		var task a2a.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			_ = rows.Close()
			return err
		}
		if task.Status.State == a2a.TaskStateWorking || task.Status.State == a2a.TaskStateSubmitted {
			now := time.Now()
			task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed, Timestamp: &now, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Execution interrupted by agent restart."))}
			raw, err = json.Marshal(task)
			if err != nil {
				_ = rows.Close()
				return err
			}
			changes = append(changes, changed{id, raw})
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if _, err := s.DB.ExecContext(ctx, `UPDATE a2a_tasks SET payload=?,version=version+1 WHERE id=?`, c.payload, c.id); err != nil {
			return err
		}
	}
	return nil
}

// Create persists a task and its accepted message IDs atomically before execution.
func (s *TaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	user, err := owner(ctx)
	if err != nil {
		return 0, err
	}
	if task == nil || task.ID == "" {
		return 0, a2a.ErrInvalidParams
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return 0, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM a2a_tasks WHERE id=?`, task.ID).Scan(&existing)
	if err == nil {
		return 0, taskstore.ErrTaskAlreadyExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO a2a_tasks VALUES(?,?,1,?)`, task.ID, user, raw); err != nil {
		return 0, err
	}
	if err := indexMessages(ctx, tx, user, task); err != nil {
		return 0, err
	}
	return 1, tx.Commit()
}
func indexMessages(ctx context.Context, tx *sql.Tx, user string, task *a2a.Task) error {
	for _, msg := range task.History {
		if msg == nil || msg.Role != a2a.MessageRoleUser {
			continue
		}
		var old string
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM a2a_messages WHERE owner=? AND message_id=?`, user, msg.ID).Scan(&old)
		if err == nil && old != string(task.ID) {
			return taskstore.ErrTaskAlreadyExists
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO a2a_messages VALUES(?,?,?) ON CONFLICT(owner,message_id) DO NOTHING`, user, msg.ID, task.ID); err != nil {
			return err
		}
	}
	return nil
}

// Update uses the SDK's optimistic version check and preserves accepted message IDs.
func (s *TaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	user, err := owner(ctx)
	if err != nil {
		return 0, err
	}
	if req == nil || req.Task == nil {
		return 0, a2a.ErrInvalidParams
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var version taskstore.TaskVersion
	if err := tx.QueryRowContext(ctx, `SELECT version FROM a2a_tasks WHERE id=? AND owner=?`, req.Task.ID, user).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return 0, a2a.ErrTaskNotFound
	} else if err != nil {
		return 0, err
	}
	if version != req.PrevVersion && req.PrevVersion != taskstore.TaskVersionMissing {
		return 0, taskstore.ErrConcurrentModification
	}
	raw, err := json.Marshal(req.Task)
	if err != nil {
		return 0, err
	}
	version++
	if _, err := tx.ExecContext(ctx, `UPDATE a2a_tasks SET payload=?,version=? WHERE id=? AND owner=?`, raw, version, req.Task.ID, user); err != nil {
		return 0, err
	}
	if err := indexMessages(ctx, tx, user, req.Task); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

// Get returns only tasks owned by the authenticated Binding.
func (s *TaskStore) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var raw []byte
	out := &taskstore.StoredTask{User: user, Task: &a2a.Task{}}
	err = s.DB.QueryRowContext(ctx, `SELECT payload,version FROM a2a_tasks WHERE id=? AND owner=?`, id, user).Scan(&raw, &out.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, out.Task); err != nil {
		return nil, err
	}
	return out, nil
}

// FindMessage returns the task that already accepted a message, even after a restart.
func (s *TaskStore) FindMessage(ctx context.Context, id string) (*a2a.Task, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var taskID a2a.TaskID
	err = s.DB.QueryRowContext(ctx, `SELECT task_id FROM a2a_messages WHERE owner=? AND message_id=?`, user, id).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	stored, err := s.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return stored.Task, nil
}

// List returns stable ID-ordered pages with message history for Agent Kourier's recovery lookup.
func (s *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	user, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	size := req.PageSize
	if size == 0 {
		size = 50
	}
	if size < 1 || size > 100 {
		return nil, a2a.ErrInvalidParams
	}
	offset := 0
	if req.PageToken != "" {
		offset, err = strconv.Atoi(req.PageToken)
		if err != nil || offset < 0 {
			return nil, a2a.ErrInvalidParams
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT payload FROM a2a_tasks WHERE owner=? ORDER BY id`, user)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var all []*a2a.Task
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		task := &a2a.Task{}
		if err := json.Unmarshal(raw, task); err != nil {
			return nil, err
		}
		if req.ContextID != "" && task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && task.Status.State != req.Status {
			continue
		}
		if req.StatusTimestampAfter != nil && task.Status.Timestamp != nil && task.Status.Timestamp.Before(*req.StatusTimestampAfter) {
			continue
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		if req.HistoryLength != nil {
			if *req.HistoryLength < 0 {
				return nil, a2a.ErrInvalidParams
			}
			if *req.HistoryLength < len(task.History) {
				task.History = task.History[len(task.History)-*req.HistoryLength:]
			}
		}
		all = append(all, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	offset = min(offset, len(all))
	end := offset + min(size, len(all)-offset)
	out := &a2a.ListTasksResponse{Tasks: all[offset:end], TotalSize: len(all), PageSize: size}
	if end < len(all) {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}
