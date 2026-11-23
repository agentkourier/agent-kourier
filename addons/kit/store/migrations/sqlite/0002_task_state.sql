-- The A2A tasks, the message IDs that started or joined them, and the contexts. Times are Unix nanoseconds.
--
-- A database the AX add-on created before the kit kept these tables, in the add-on's migration 0001, in this same
-- layout: this migration then adopts them and their rows as they are (IF NOT EXISTS), and adds the contexts' ref.

-- payload is the task as JSON; the other columns are what the store filters and orders on. version counts updates, for
-- optimistic concurrency. owner is the caller that created the task: a task is visible to its owner alone.
CREATE TABLE IF NOT EXISTS a2a_tasks (
	id         TEXT PRIMARY KEY,
	context_id TEXT NOT NULL,
	owner      TEXT NOT NULL,
	version    INTEGER NOT NULL,
	state      TEXT NOT NULL,
	status_at  INTEGER,
	updated_at INTEGER NOT NULL,
	payload    BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS a2a_tasks_owner_context ON a2a_tasks (owner, context_id, updated_at);
CREATE INDEX IF NOT EXISTS a2a_tasks_state ON a2a_tasks (state, updated_at);
CREATE INDEX IF NOT EXISTS a2a_tasks_owner_updated ON a2a_tasks (owner, updated_at, id);

-- The message IDs of each owner's user messages, with the task each started or joined, recorded in the transaction
-- that writes the task, so that a resent message joins its task after a restart too.
CREATE TABLE IF NOT EXISTS message_ids (
	owner      TEXT NOT NULL,
	message_id TEXT NOT NULL,
	task_id    TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (owner, message_id)
);
CREATE INDEX IF NOT EXISTS message_ids_task ON message_ids (task_id);

-- Each owner's A2A contexts: when each was last active, and ref, the add-on's own handle on it (a backend session's ID,
-- or empty). It is kept apart from a2a_tasks because the task retention deletes settled tasks long before a context
-- goes idle. ref is added below rather than in the CREATE, so that a table this migration adopts gets it too.
CREATE TABLE IF NOT EXISTS contexts (
	owner       TEXT NOT NULL,
	context_id  TEXT NOT NULL,
	last_active INTEGER NOT NULL,
	PRIMARY KEY (owner, context_id)
);
CREATE INDEX IF NOT EXISTS contexts_last_active ON contexts (last_active);
ALTER TABLE contexts ADD COLUMN ref TEXT NOT NULL DEFAULT '';
