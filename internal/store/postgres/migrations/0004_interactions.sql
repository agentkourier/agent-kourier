CREATE TABLE interactions (
    id          text PRIMARY KEY,
    connection  text NOT NULL,
    channel     text NOT NULL,
    thread_ts   text NOT NULL,
    task_id     text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('ask_user', 'tool_approval', 'nested')),
    state       text NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'answered', 'timed_out', 'cancelled')),
    request     bytea NOT NULL,
    message_ref text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL,
    deadline    timestamptz NOT NULL CHECK (deadline > created_at),
    answer      bytea NOT NULL DEFAULT ''::bytea,
    decider     text NOT NULL DEFAULT '',
    decided_at  timestamptz -- NULL until decided
);
CREATE INDEX interactions_due ON interactions (deadline) WHERE state = 'pending';
