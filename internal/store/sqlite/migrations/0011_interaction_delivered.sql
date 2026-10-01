-- When an answer was handed to the session (0 until it was). An answer decided before this column has
-- no record either way, and none is made out to be undelivered: it is taken as delivered when it was decided.
ALTER TABLE interactions ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0;
UPDATE interactions SET delivered_at = decided_at WHERE state = 'answered';
