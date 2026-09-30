CREATE TABLE interactions (
    id          TEXT PRIMARY KEY,
    connection  TEXT NOT NULL,
    channel     TEXT NOT NULL,
    thread_ts   TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('ask_user', 'tool_approval', 'nested')),
    state       TEXT NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'answered', 'timed_out', 'cancelled')),
    request     BLOB NOT NULL,
    message_ref TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    deadline    INTEGER NOT NULL CHECK (deadline > created_at),
    answer      BLOB NOT NULL DEFAULT x'',
    decider     TEXT NOT NULL DEFAULT '',
    decided_at  INTEGER NOT NULL DEFAULT 0 -- 0 until decided
);
CREATE INDEX interactions_due ON interactions (deadline) WHERE state = 'pending';
