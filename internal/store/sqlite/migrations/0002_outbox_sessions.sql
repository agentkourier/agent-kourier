CREATE TABLE outbox (
    id          TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL,
    role        TEXT NOT NULL,
    channel     TEXT NOT NULL,
    thread_ts   TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    message_ref TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_unposted ON outbox (created_at, id) WHERE message_ref = '';

CREATE TABLE sessions (
    connection    TEXT NOT NULL,
    channel       TEXT NOT NULL,
    thread_ts     TEXT NOT NULL,
    context_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    binding       TEXT NOT NULL,
    last_activity INTEGER NOT NULL,
    PRIMARY KEY (connection, channel, thread_ts)
);
