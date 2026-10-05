-- Feedback (spec 8.7, 9): a person's verdict on a message of an investigation, one row for each person and message, replaced
-- when they change their mind. (connection, channel, thread_ts) is the session and turn_id the turn whose output is the rated
-- message, so a thread's several answers are rated apart. binding, agent and alert_name are kept as they were at the first
-- vote; alert_name is empty for a thread no alert started. kind is 'investigation' for the final message of a thread that a
-- trigger or an alert started, and 'answer' for the answer to a person. No text a person wrote is stored, and no retention
-- purge deletes a row (docs/data.md). first_at and voted_at are the first and the latest vote.
-- Additive: a new table, and a column that an older binary neither reads nor writes.
CREATE TABLE feedback (
    connection TEXT NOT NULL,
    channel    TEXT NOT NULL,
    thread_ts  TEXT NOT NULL,
    turn_id    TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    binding    TEXT NOT NULL,
    agent      TEXT NOT NULL,
    alert_name TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL CHECK (kind IN ('investigation', 'answer')),
    verdict    TEXT NOT NULL CHECK (verdict IN ('useful', 'not_useful')),
    first_at   INTEGER NOT NULL,
    voted_at   INTEGER NOT NULL,
    PRIMARY KEY (connection, channel, thread_ts, turn_id, user_id)
);

-- The alert a thread was started for, and the agent of its Binding when it was made, which the rating of its messages needs when it
-- is cast, long after the start and whatever the Binding says by then.
ALTER TABLE sessions ADD COLUMN alert_name TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN agent_name TEXT NOT NULL DEFAULT '';
