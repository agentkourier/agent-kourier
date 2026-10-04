-- The starts of a Binding's botMessage triggers, for their limits (spec 8.2, D26, D31): one row for each alert message
-- that was decided to start an investigation, or that a limit stopped. connection, channel and thread_ts are the alert's
-- own message, which is also the thread of the session it starts, so a retried start finds its row and is counted once,
-- and the row can be joined to the session and its queued turn to tell whether the investigation is still running.
-- trigger_index is the trigger's place in the Binding's chat.triggers.
-- limited is '' for a start, and otherwise the limit that stopped it. A limited row belongs to a digest window
-- [window_start, window_end), which opens with the first limited start after the last digest and lasts the trigger's
-- rate window; digested is 1 once the digest line of that window was queued. Rows are kept for 24 hours, the longest
-- any window counts, and then purged. No alert text is stored.
CREATE TABLE trigger_runs (
    connection    TEXT NOT NULL,
    channel       TEXT NOT NULL,
    thread_ts     TEXT NOT NULL,
    binding       TEXT NOT NULL,
    trigger_index INTEGER NOT NULL,
    at            INTEGER NOT NULL,
    limited       TEXT NOT NULL DEFAULT '' CHECK (limited IN ('', 'rate', 'concurrent', 'daily')),
    window_start  INTEGER NOT NULL DEFAULT 0,
    window_end    INTEGER NOT NULL DEFAULT 0,
    digested      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (connection, channel, thread_ts)
);
CREATE INDEX trigger_runs_at ON trigger_runs (binding, trigger_index, at) WHERE limited = '';
CREATE INDEX trigger_runs_undigested ON trigger_runs (window_end) WHERE limited <> '' AND digested = 0;
CREATE INDEX trigger_runs_purge ON trigger_runs (at);
