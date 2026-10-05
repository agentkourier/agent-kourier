-- sequence is assigned by the database, so order never depends on a caller's clock. An identity column
-- is never given again after a delete, as AUTOINCREMENT is not on SQLite.
--
-- An identity value is taken when a row is inserted, and the row is shown when its transaction commits,
-- so a reader that walks a session's queue by sequence could see 6 before 5 and never see 5. The store
-- takes a per-session advisory lock around each enqueue (see package postgres), so that within a
-- session the sequence order is the commit order.
CREATE TABLE reply_queue (
    sequence    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id          text NOT NULL UNIQUE,
    connection  text NOT NULL,
    channel     text NOT NULL,
    thread_ts   text NOT NULL,
    user_id     text NOT NULL,
    message_ts  text NOT NULL,
    text        text NOT NULL, -- cleared once delivered
    enqueued_at timestamptz NOT NULL,
    -- NULL until delivered. A delivered row stays as a tombstone, so a replayed chat event ID cannot
    -- be queued again.
    delivered_at timestamptz
);
CREATE INDEX reply_queue_head ON reply_queue (connection, channel, thread_ts, sequence) WHERE delivered_at IS NULL;
