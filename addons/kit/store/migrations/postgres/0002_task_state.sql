-- The SQLite migration of the same number, in Postgres types. Times are Unix nanoseconds in a bigint, as on SQLite,
-- so that both stores run the same SQL. payload is json, not jsonb: jsonb refuses the NUL a model's or a tool's output
-- may hold.
--
-- A database the AX add-on created before the kit kept these tables, in the add-on's migration 0001, in this same
-- layout: this migration then adopts them and their rows as they are (IF NOT EXISTS), and adds the contexts' ref.
CREATE TABLE IF NOT EXISTS a2a_tasks (
	id         text PRIMARY KEY,
	context_id text NOT NULL,
	owner      text NOT NULL,
	version    bigint NOT NULL,
	state      text NOT NULL,
	status_at  bigint,
	updated_at bigint NOT NULL,
	payload    json NOT NULL
);
CREATE INDEX IF NOT EXISTS a2a_tasks_owner_context ON a2a_tasks (owner, context_id, updated_at);
CREATE INDEX IF NOT EXISTS a2a_tasks_state ON a2a_tasks (state, updated_at);
CREATE INDEX IF NOT EXISTS a2a_tasks_owner_updated ON a2a_tasks (owner, updated_at, id);

CREATE TABLE IF NOT EXISTS message_ids (
	owner      text NOT NULL,
	message_id text NOT NULL,
	task_id    text NOT NULL,
	created_at bigint NOT NULL,
	PRIMARY KEY (owner, message_id)
);
CREATE INDEX IF NOT EXISTS message_ids_task ON message_ids (task_id);

CREATE TABLE IF NOT EXISTS contexts (
	owner       text NOT NULL,
	context_id  text NOT NULL,
	last_active bigint NOT NULL,
	PRIMARY KEY (owner, context_id)
);
CREATE INDEX IF NOT EXISTS contexts_last_active ON contexts (last_active);
ALTER TABLE contexts ADD COLUMN ref text NOT NULL DEFAULT '';
