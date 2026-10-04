-- The thread courier opened for each key of a Binding's botMessage trigger (spec 8.2), kept after the
-- thread closes so that the cooldown outlives the close. key_hash is a hash of the key, which is untrusted
-- text from a bot's message and is never stored. closed_at is 0 while the thread is open, as abandoned_at
-- is 0 for an outbox row that was not abandoned. cooldown_until is 0 until the thread's turn has started,
-- so a turn that could not start leaves no cooldown behind.
CREATE TABLE trigger_threads (
    binding        TEXT NOT NULL,
    key_hash       TEXT NOT NULL,
    thread         TEXT NOT NULL,
    opened         INTEGER NOT NULL,
    last_seen      INTEGER NOT NULL,
    closed_at      INTEGER NOT NULL DEFAULT 0,
    cooldown_until INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (binding, key_hash)
);
