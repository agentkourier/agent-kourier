-- This migration added the leadership row, by which replicas elect a leader. The kit's migration 0001 has it now, and
-- runs before the adapter's: on a database this migration already built, the kit's adopts the table and its row as
-- they are. It stays, as a statement that does nothing, so that the version list does not change.
SELECT 1;
