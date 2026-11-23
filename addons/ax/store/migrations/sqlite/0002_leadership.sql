-- The Postgres migration of the same number added the leadership row, which the kit's migrations hold now. SQLite is
-- one replica's store (the file lock keeps a second one out), so it elects none and has no such row: this migration
-- only keeps the two lists in step.
SELECT 1;
