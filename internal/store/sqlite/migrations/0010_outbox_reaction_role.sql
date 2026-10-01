-- The role set gains 'reaction' (spec 8.6). The triggers of 0003 name the set, so they are
-- replaced; SQLite cannot change a trigger's condition in place.
DROP TRIGGER outbox_role_insert;
DROP TRIGGER outbox_role_update;

CREATE TRIGGER outbox_role_insert BEFORE INSERT ON outbox
WHEN NEW.role NOT IN ('root', 'reply', 'digest', 'notice', 'reaction')
BEGIN
    SELECT RAISE(ABORT, 'invalid outbox role');
END;

CREATE TRIGGER outbox_role_update BEFORE UPDATE OF role ON outbox
WHEN NEW.role NOT IN ('root', 'reply', 'digest', 'notice', 'reaction')
BEGIN
    SELECT RAISE(ABORT, 'invalid outbox role');
END;
