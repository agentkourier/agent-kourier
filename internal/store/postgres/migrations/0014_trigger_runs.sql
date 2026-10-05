-- The starts of a Binding's botMessage triggers, for their limits (spec 8.2, D26, D31): one row for each alert
-- message that was decided to start an investigation, or that a limit stopped. connection, channel and thread_ts are the alert's
-- own message, which is also the thread of the session it starts, so a retried start finds its row and is counted once,
-- and the row can be joined to the session and its queued turn to tell whether the investigation is still running.
-- trigger_index is the trigger's place in the Binding's chat.triggers.
-- limited is '' for a start, and otherwise the limit that stopped it. A limited row belongs to a digest window
-- [window_start, window_end), which opens with the first limited start after the last digest and lasts the trigger's
-- rate window (both NULL for a start); digested is true once the digest line of that window was queued. Rows are kept
-- for 24 hours, the longest any window counts, and then purged. No alert text is stored.
CREATE TABLE trigger_runs (
    connection    text NOT NULL,
    channel       text NOT NULL,
    thread_ts     text NOT NULL,
    binding       text NOT NULL,
    trigger_index integer NOT NULL,
    at            timestamptz NOT NULL,
    limited       text NOT NULL DEFAULT '' CHECK (limited IN ('', 'rate', 'concurrent', 'daily')),
    window_start  timestamptz,
    window_end    timestamptz,
    digested      boolean NOT NULL DEFAULT false,
    PRIMARY KEY (connection, channel, thread_ts)
);
CREATE INDEX trigger_runs_at ON trigger_runs (binding, trigger_index, at) WHERE limited = '';
-- The open window of a trigger is found, and a window is marked digested, by Binding and trigger among the few rows that
-- are stopped and not yet digested; the digests due are read from the same small index.
CREATE INDEX trigger_runs_undigested ON trigger_runs (binding, trigger_index, window_end) WHERE limited <> '' AND NOT digested;
CREATE INDEX trigger_runs_purge ON trigger_runs (at);
