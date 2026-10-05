-- The turn whose output the active task belongs to, recorded with the task, so that a recovery adopts that turn's stream.
-- Additive: an older binary neither reads nor writes them.
ALTER TABLE sessions ADD COLUMN turn_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_user TEXT NOT NULL DEFAULT '';
