-- SQLite enforces the role set with triggers (it cannot add a CHECK to a column); Postgres has the
-- constraint itself. 0010 replaces it when the set grows.
ALTER TABLE outbox ADD CONSTRAINT outbox_role_check CHECK (role IN ('root', 'reply', 'digest', 'notice'));
