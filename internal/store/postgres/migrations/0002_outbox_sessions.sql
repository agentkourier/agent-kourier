CREATE TABLE outbox (
    id          text PRIMARY KEY,
    event_id    text NOT NULL,
    role        text NOT NULL,
    channel     text NOT NULL,
    thread_ts   text NOT NULL,
    created_at  timestamptz NOT NULL,
    message_ref text NOT NULL DEFAULT ''
);
CREATE INDEX outbox_unposted ON outbox (created_at, id) WHERE message_ref = '';

CREATE TABLE sessions (
    connection    text NOT NULL,
    channel       text NOT NULL,
    thread_ts     text NOT NULL,
    context_id    text NOT NULL,
    task_id       text NOT NULL,
    binding       text NOT NULL,
    last_activity timestamptz NOT NULL,
    PRIMARY KEY (connection, channel, thread_ts)
);
