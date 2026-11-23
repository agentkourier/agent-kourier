-- The A2A tasks. payload is the task as JSON; the other columns are what the store filters and orders on. Times are
-- Unix nanoseconds. version counts updates, for optimistic concurrency. owner is the caller that created the task: a
-- task is visible to its owner alone.
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
CREATE INDEX a2a_tasks_owner_context ON a2a_tasks (owner, context_id, updated_at);
CREATE INDEX a2a_tasks_state ON a2a_tasks (state, updated_at);
CREATE INDEX a2a_tasks_owner_updated ON a2a_tasks (owner, updated_at, id);

-- The message IDs of each owner's user messages, with the task each started or joined, so that a resent message
-- joins its task after a restart too.
CREATE TABLE message_ids (
	owner      TEXT NOT NULL,
	message_id TEXT NOT NULL,
	task_id    TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (owner, message_id)
);
CREATE INDEX message_ids_task ON message_ids (task_id);

-- When each owner's A2A context last had a task created or updated. A context idle past SESSION_IDLE_TTL loses its
-- ADK session (the sessions and events tables, which ADK's gorm migration keeps) and its tasks. It is kept apart from
-- a2a_tasks because TASK_RETENTION deletes settled tasks long before a context goes idle.
CREATE TABLE contexts (
	owner       TEXT NOT NULL,
	context_id  TEXT NOT NULL,
	last_active INTEGER NOT NULL,
	PRIMARY KEY (owner, context_id)
);
CREATE INDEX contexts_last_active ON contexts (last_active);
