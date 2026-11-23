-- The SQLite migration of the same number, in Postgres types. Times are Unix nanoseconds in a bigint, as on SQLite,
-- so that both stores run the same SQL. payload is json, not jsonb: jsonb refuses the NUL a tool's output may hold.
CREATE TABLE a2a_tasks (
	id         text PRIMARY KEY,
	context_id text NOT NULL,
	owner      text NOT NULL,
	version    bigint NOT NULL,
	state      text NOT NULL,
	status_at  bigint,
	updated_at bigint NOT NULL,
	payload    json NOT NULL
);
CREATE INDEX a2a_tasks_context ON a2a_tasks (context_id, updated_at);
CREATE INDEX a2a_tasks_state ON a2a_tasks (state, updated_at);
CREATE INDEX a2a_tasks_updated ON a2a_tasks (updated_at, id);

CREATE TABLE message_ids (
	message_id text PRIMARY KEY,
	task_id    text NOT NULL,
	created_at bigint NOT NULL
);
CREATE INDEX message_ids_task ON message_ids (task_id);

CREATE TABLE context_sessions (
	context_id  text PRIMARY KEY,
	session_id  text NOT NULL,
	last_active bigint NOT NULL
);
