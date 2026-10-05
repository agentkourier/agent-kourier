-- The role set gains 'reaction' (spec 8.6): the constraint of 0003 is replaced.
ALTER TABLE outbox DROP CONSTRAINT outbox_role_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_role_check CHECK (role IN ('root', 'reply', 'digest', 'notice', 'reaction'));
