-- The task the recorded turn was recorded for. A binary from before the turn was recorded starts a task without touching the
-- turn, so a turn is the task's only while this names the task. Additive.
ALTER TABLE sessions ADD COLUMN turn_task text NOT NULL DEFAULT '';
