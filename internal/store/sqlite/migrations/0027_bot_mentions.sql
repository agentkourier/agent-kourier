-- A bot's mention in a thread is a start that its own message did not open, so a run keeps the session's thread beside
-- its message, and a turn says whether it carries the thread. Both are additive: an older binary ignores them.
ALTER TABLE trigger_runs ADD COLUMN session_thread_ts TEXT NOT NULL DEFAULT '';
ALTER TABLE reply_queue ADD COLUMN whole_thread INTEGER NOT NULL DEFAULT 0;
