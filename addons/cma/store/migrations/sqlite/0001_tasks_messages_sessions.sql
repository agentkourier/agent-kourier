-- The A2A tasks. payload is the task as JSON; the other columns are what the store filters and orders on. Times are
-- Unix nanoseconds. version counts updates, for optimistic concurrency. owner is the caller that created the task.
CREATE TABLE a2a_tasks (
	id         TEXT PRIMARY KEY,
	context_id TEXT NOT NULL,
	owner      TEXT NOT NULL,
	version    INTEGER NOT NULL,
	state      TEXT NOT NULL,
	status_at  INTEGER,
	updated_at INTEGER NOT NULL,
	payload    BLOB NOT NULL
);
CREATE INDEX a2a_tasks_context ON a2a_tasks (context_id, updated_at);
CREATE INDEX a2a_tasks_state ON a2a_tasks (state, updated_at);
CREATE INDEX a2a_tasks_updated ON a2a_tasks (updated_at, id);

-- The message IDs that started or joined a task, so that a resent message joins its task after a restart too.
CREATE TABLE message_ids (
	message_id TEXT PRIMARY KEY,
	task_id    TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX message_ids_task ON message_ids (task_id);

-- Each A2A context's CMA session.
CREATE TABLE context_sessions (
	context_id  TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	last_active INTEGER NOT NULL
);
