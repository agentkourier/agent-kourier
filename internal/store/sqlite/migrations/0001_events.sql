CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    binding_key     TEXT NOT NULL,
    payload         BLOB NOT NULL,
    received_at     INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processing', 'done', 'failed', 'dead')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_status_next_attempt ON events (status, next_attempt_at);
