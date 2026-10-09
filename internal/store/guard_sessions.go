package store

import (
	"context"
	"fmt"
	"time"
)

// UpsertSession implements Sessions.
func (g *Guarded) UpsertSession(ctx context.Context, sess Session) error {
	return g.run(ctx, "agentkourier.store.UpsertSession", func(ctx context.Context) error {
		stamp(ctx, &sess.TraceParent, &sess.TraceState)
		if !validKey(sess.Key) || sess.Binding == "" || sess.LastActivity.IsZero() {
			return fmt.Errorf("upsert session %+v: key, Binding and LastActivity are required: %w", sess.Key, ErrInvalid)
		}
		return g.a.UpsertSession(ctx, sess)
	})
}

// GetSession implements Sessions.
func (g *Guarded) GetSession(ctx context.Context, key ConversationKey, ttl time.Duration, now time.Time) (sess Session, err error) {
	err = g.run(ctx, "agentkourier.store.GetSession", func(ctx context.Context) error {
		if !validKey(key) || ttl <= 0 || now.IsZero() {
			return fmt.Errorf("get session %+v: key, positive ttl and now are required: %w", key, ErrInvalid)
		}
		var err error
		sess, err = g.a.GetSession(ctx, key, ttl, now)
		return err
	})
	return sess, err
}

// TouchSession implements Sessions.
func (g *Guarded) TouchSession(ctx context.Context, key ConversationKey, at time.Time) error {
	return g.run(ctx, "agentkourier.store.TouchSession", func(ctx context.Context) error {
		if !validKey(key) || at.IsZero() {
			return fmt.Errorf("touch session %+v: key and time are required: %w", key, ErrInvalid)
		}
		return g.a.TouchSession(ctx, key, at)
	})
}

// SetSessionTask implements SessionTasks.
func (g *Guarded) SetSessionTask(ctx context.Context, key ConversationKey, contextID, taskID string, turn TaskTurn) error {
	return g.run(ctx, "agentkourier.store.SetSessionTask", func(ctx context.Context) error {
		if !validKey(key) {
			return fmt.Errorf("set session task %+v: the key is incomplete: %w", key, ErrInvalid)
		}
		return g.a.SetSessionTask(ctx, key, contextID, taskID, turn)
	})
}

// ResetSession implements SessionTasks.
func (g *Guarded) ResetSession(ctx context.Context, key ConversationKey) error {
	return g.run(ctx, "agentkourier.store.ResetSession", func(ctx context.Context) error {
		if !validKey(key) {
			return fmt.Errorf("reset session %+v: the key is incomplete: %w", key, ErrInvalid)
		}
		return g.a.ResetSession(ctx, key)
	})
}

// ListActiveSessions implements Recovery.
func (g *Guarded) ListActiveSessions(ctx context.Context) (sessions []Session, err error) {
	err = g.run(ctx, "agentkourier.store.ListActiveSessions", func(ctx context.Context) error {
		var err error
		sessions, err = g.a.ListActiveSessions(ctx)
		return err
	})
	return sessions, err
}

// ListQueuedSessions implements Recovery.
func (g *Guarded) ListQueuedSessions(ctx context.Context) (keys []ConversationKey, err error) {
	err = g.run(ctx, "agentkourier.store.ListQueuedSessions", func(ctx context.Context) error {
		var err error
		keys, err = g.a.ListQueuedSessions(ctx)
		return err
	})
	return keys, err
}

// PurgeExpiredSessions implements SessionRetention.
func (g *Guarded) PurgeExpiredSessions(ctx context.Context, h SessionHorizons, limit int) (purged PurgedSessions, err error) {
	err = g.run(ctx, "agentkourier.store.PurgeExpiredSessions", func(ctx context.Context) error {
		if !h.Valid() || !validLimit(limit) {
			return fmt.Errorf("purge expired sessions: every horizon and a limit of 1 to %d are required: %w", MaxLimit, ErrInvalid)
		}
		var err error
		purged, err = g.a.PurgeExpiredSessions(ctx, h, limit)
		return err
	})
	return purged, err
}

// ListStaleTasks implements StaleTasks.
func (g *Guarded) ListStaleTasks(ctx context.Context, h SessionHorizons, limit int) (stale []StaleTask, err error) {
	err = g.run(ctx, "agentkourier.store.ListStaleTasks", func(ctx context.Context) error {
		if !h.Valid() || !validLimit(limit) {
			return fmt.Errorf("list stale tasks: every horizon and a limit of 1 to %d are required: %w", MaxLimit, ErrInvalid)
		}
		var err error
		stale, err = g.a.ListStaleTasks(ctx, h, limit)
		return err
	})
	return stale, err
}

// EndStaleTask implements StaleTasks.
func (g *Guarded) EndStaleTask(ctx context.Context, h SessionHorizons, t StaleTask, now time.Time) (ended EndedStaleTask, err error) {
	err = g.run(ctx, "agentkourier.store.EndStaleTask", func(ctx context.Context) error {
		if !h.Valid() || !validKey(t.Key) || t.TaskID == "" || now.IsZero() {
			return fmt.Errorf("end stale task %+v: every horizon, a complete key, a task and a time are required: %w", t.Key, ErrInvalid)
		}
		var err error
		ended, err = g.a.EndStaleTask(ctx, h, t, now)
		return err
	})
	return ended, err
}
