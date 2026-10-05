-- The thread courier opened for each key of a Binding's botMessage trigger (spec 8.2), kept after the
-- thread closes so that the cooldown outlives the close. key_hash is a hash of the key, which is untrusted
-- text from a bot's message and is never stored. closed_at is NULL while the thread is open, and
-- cooldown_until is NULL until the thread's turn has started, so a turn that could not start leaves no
-- cooldown behind. (SQLite stores 0 for both.)
CREATE TABLE trigger_threads (
    binding        text NOT NULL,
    key_hash       text NOT NULL,
    thread         text NOT NULL,
    opened         timestamptz NOT NULL,
    last_seen      timestamptz NOT NULL,
    closed_at      timestamptz,
    cooldown_until timestamptz,
    PRIMARY KEY (binding, key_hash)
);
