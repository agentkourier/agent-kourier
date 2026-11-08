-- The outbox and the presses are keyed by the chat connection (the app) they belong to. A channel ID names a channel only
-- within one app, so with more than one connection a row keyed by channel and thread alone is ambiguous: expiring one app's
-- session would delete the settled outbox rows of another's in the same thread, and the press cap would be shared.
-- An empty connection is a row written before this migration, or by an older binary after a rollback: it belongs to the
-- broker's own connection, and the leader gives it that at every start (AttributeConnection). Additive: an older binary
-- ignores the column, and writes its rows with the empty default.
ALTER TABLE outbox ADD COLUMN connection TEXT NOT NULL DEFAULT '';
ALTER TABLE interaction_presses ADD COLUMN connection TEXT NOT NULL DEFAULT '';
-- The purge of a session's outbox rows, the check that none is pending, and AttributeConnection (the rows with an empty
-- connection) read by the connection first.
DROP INDEX outbox_thread;
CREATE INDEX outbox_thread ON outbox (connection, channel, thread_ts);
-- The chat connections this broker served, as of the last time it recorded them: a restarted broker compares them with the
-- ones it serves now, to tell which it stopped serving. Nothing reads or writes the table yet. served_since is when the
-- connection first entered the set.
CREATE TABLE served_connections (
    connection   TEXT PRIMARY KEY,
    served_since INTEGER NOT NULL
);
