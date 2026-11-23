package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// The agent kept its tasks, before it was an add-on, in a SQLite layout of its own beside ADK's tables: a2a_tasks(id,
// owner, version, payload) and a2a_messages(owner, message_id, task_id), with no schema_migrations. A file in that
// layout has those two tables renamed before the migrations run, the kit's included, and their rows imported after,
// so that a question waiting across the upgrade can still be answered. ADK's tables are ADK's own, and carry over as
// they are.

// setLegacyAside renames the old layout's tables, when the file is in it, and reports whether there are renamed
// tables to import: the ones it renamed, or ones a start cut short left. It is the SQLite store's kitstore.Options
// Prepare, so it runs in a transaction of its own before the kit's migrations, which would otherwise find the old
// layout's a2a_tasks. Its write, and importLegacy's, is not fenced: SQLite has no fence.
func setLegacyAside(ctx context.Context, q kitstore.Querier) (bool, error) {
	has := func(table string) (bool, error) {
		var n int
		err := q.QueryRow(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		return n > 0, err
	}
	tasks, err := has("a2a_tasks")
	if err != nil {
		return false, fmt.Errorf("set the old task tables aside: %w", err)
	}
	// The old layout is known by its a2a_tasks, which has no context_id: a version table would not tell it from the
	// kit's tables of a first start cut short before the agent's migrations made theirs.
	var contextIDs int
	if tasks {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('a2a_tasks') WHERE name = 'context_id'`).Scan(&contextIDs); err != nil {
			return false, fmt.Errorf("set the old task tables aside: %w", err)
		}
	}
	if tasks && contextIDs == 0 {
		for _, rename := range []struct{ from, to string }{{"a2a_tasks", "legacy_a2a_tasks"}, {"a2a_messages", "legacy_a2a_messages"}} {
			ok, err := has(rename.from)
			if err != nil {
				return false, fmt.Errorf("set the old task tables aside: %w", err)
			}
			if !ok {
				if _, err := q.Exec(ctx, `CREATE TABLE `+rename.to+` (owner TEXT NOT NULL, message_id TEXT NOT NULL, task_id TEXT NOT NULL)`); err != nil {
					return false, fmt.Errorf("set the old task tables aside: %w", err)
				}
				continue
			}
			if _, err := q.Exec(ctx, `ALTER TABLE `+rename.from+` RENAME TO `+rename.to); err != nil {
				return false, fmt.Errorf("set the old task tables aside: %w", err)
			}
		}
	}
	legacy, err := has("legacy_a2a_tasks")
	if err != nil {
		return false, fmt.Errorf("set the old task tables aside: %w", err)
	}
	return legacy, nil
}

// importLegacy copies the renamed tables' rows into the store's, each task as updated now and its context as active
// now, and drops them, in one transaction. A row the store already has is kept as it is.
func (s *Store) importLegacy(ctx context.Context) error {
	now := s.now().UnixNano()
	imported := 0
	err := s.Write(ctx, func(q kitstore.Querier) error {
		type row struct {
			id, owner string
			version   int64
			raw       []byte
		}
		var rows []row
		err := q.Query(ctx, `SELECT id, owner, version, payload FROM legacy_a2a_tasks ORDER BY id`, nil, func(scan func(...any) error) error {
			var r row
			if err := scan(&r.id, &r.owner, &r.version, &r.raw); err != nil {
				return err
			}
			rows = append(rows, r)
			return nil
		})
		if err != nil {
			return err
		}
		for _, r := range rows {
			task := &a2a.Task{}
			if err := json.Unmarshal(r.raw, task); err != nil {
				return fmt.Errorf("task %s: decode task: %w", r.id, err)
			}
			var statusAt *int64
			if ts := task.Status.Timestamp; ts != nil {
				n := ts.UnixNano()
				statusAt = &n
			}
			if _, err := q.Exec(ctx, `INSERT INTO a2a_tasks (id, context_id, owner, version, state, status_at, updated_at, payload)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
				r.id, task.ContextID, r.owner, r.version, string(task.Status.State), statusAt, now, r.raw); err != nil {
				return fmt.Errorf("task %s: %w", r.id, err)
			}
			if task.ContextID != "" {
				if _, err := q.Exec(ctx, `INSERT INTO contexts (owner, context_id, last_active) VALUES (?, ?, ?)
					ON CONFLICT (owner, context_id) DO NOTHING`, r.owner, task.ContextID, now); err != nil {
					return err
				}
			}
		}
		if _, err := q.Exec(ctx, `INSERT INTO message_ids (owner, message_id, task_id, created_at)
			SELECT owner, message_id, task_id, ? FROM legacy_a2a_messages WHERE true ON CONFLICT (owner, message_id) DO NOTHING`, now); err != nil {
			return fmt.Errorf("message IDs: %w", err)
		}
		for _, table := range []string{"legacy_a2a_messages", "legacy_a2a_tasks"} {
			if _, err := q.Exec(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
				return err
			}
		}
		imported = len(rows)
		return nil
	})
	if err != nil {
		return fmt.Errorf("import the tasks of the old layout: %w", err)
	}
	s.log.InfoContext(ctx, "imported the tasks of the agent's old SQLite layout", "tasks", imported)
	return nil
}
