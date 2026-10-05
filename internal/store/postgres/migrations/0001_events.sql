CREATE TABLE events (
    id              text PRIMARY KEY,
    kind            text NOT NULL,
    binding_key     text NOT NULL,
    payload         bytea NOT NULL,
    received_at     timestamptz NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processing', 'done', 'failed', 'dead')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    last_error      text NOT NULL DEFAULT ''
);
-- ClaimEvent reads the claimable events in the order they are due; the partial index holds only those, so
-- it stays small however many events are done.
CREATE INDEX events_claimable ON events (next_attempt_at, received_at) WHERE status IN ('pending', 'processing');
