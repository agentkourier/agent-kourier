-- Row retention (data protection, DP-3): the sweep that deletes sessions long past their expiry, and the rows that
-- belong to them, finds a session by its last activity and the rest by the session's key, a thread, or a reply's ID.
-- None of those columns had an index (a delivered reply, a settled interaction and a posted outbox row were never
-- looked up by them). Additive: indexes only; a binary from before the release reads and writes as it did.
CREATE INDEX sessions_last_activity ON sessions (last_activity);
CREATE INDEX reply_queue_thread ON reply_queue (connection, channel, thread_ts);
CREATE INDEX interactions_thread ON interactions (connection, channel, thread_ts);
CREATE INDEX outbox_thread ON outbox (channel, thread_ts);
CREATE INDEX outbox_event ON outbox (event_id);
