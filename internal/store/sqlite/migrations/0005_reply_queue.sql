-- sequence is assigned by the database, so order never depends on a caller's
-- clock. AUTOINCREMENT keeps it strictly increasing even after deletes.
CREATE TABLE reply_queue (
    sequence    INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT NOT NULL UNIQUE,
    connection  TEXT NOT NULL,
    channel     TEXT NOT NULL,
    thread_ts   TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    message_ts  TEXT NOT NULL,
    text        TEXT NOT NULL, -- cleared once delivered
    enqueued_at INTEGER NOT NULL,
    -- NULL until delivered. A delivered row stays as a tombstone, so a replayed
    -- Slack event ID cannot be queued again.
    delivered_at INTEGER
);
CREATE INDEX reply_queue_head ON reply_queue (connection, channel, thread_ts, sequence) WHERE delivered_at IS NULL;
