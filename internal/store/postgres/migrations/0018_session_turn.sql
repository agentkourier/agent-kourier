-- The turn whose output the active task belongs to, recorded with the task, so that a recovery adopts that turn's stream.
-- Additive: an older binary neither reads nor writes them.
ALTER TABLE sessions ADD COLUMN turn_id text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_user text NOT NULL DEFAULT '';
