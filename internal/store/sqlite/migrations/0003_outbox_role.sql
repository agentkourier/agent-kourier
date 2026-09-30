-- SQLite cannot add a CHECK to an existing column with ALTER TABLE. A trigger
-- enforces the role set without rebuilding the table, copying rows, or
-- recreating the partial index.
CREATE TRIGGER outbox_role_insert BEFORE INSERT ON outbox
WHEN NEW.role NOT IN ('root', 'reply', 'digest', 'notice')
BEGIN
    SELECT RAISE(ABORT, 'invalid outbox role');
END;

CREATE TRIGGER outbox_role_update BEFORE UPDATE OF role ON outbox
WHEN NEW.role NOT IN ('root', 'reply', 'digest', 'notice')
BEGIN
    SELECT RAISE(ABORT, 'invalid outbox role');
END;
