-- A launch can outlive A2A task history, so there is no task foreign key.
CREATE TABLE ax_launches (
    task_id TEXT PRIMARY KEY,
    task_name TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    result_id TEXT NOT NULL UNIQUE,
    task_uid TEXT NOT NULL DEFAULT '',
    actor_uid TEXT NOT NULL DEFAULT '',
    invocation_id TEXT NOT NULL DEFAULT '',
    deadline BIGINT NOT NULL,
    drain_until BIGINT NOT NULL DEFAULT 0,
    resume_sent INTEGER NOT NULL DEFAULT 0,
    decision TEXT NOT NULL DEFAULT '',
    receipt TEXT NOT NULL DEFAULT '',
    exit_code INTEGER NOT NULL DEFAULT -1,
    output TEXT NOT NULL DEFAULT '',
    truncated INTEGER NOT NULL DEFAULT 0,
    cleanup_at BIGINT NOT NULL DEFAULT 0,
    suspended INTEGER NOT NULL DEFAULT 0,
    clean INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ax_launches_cleanup ON ax_launches (clean, cleanup_at, task_id);
CREATE INDEX ax_launches_name ON ax_launches (task_name);
