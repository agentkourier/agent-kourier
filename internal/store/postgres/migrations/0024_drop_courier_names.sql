-- The names that carried the old project name go. Migration 0023 added the audit purge functions and the retention
-- setting under the Agent Kourier names beside the old ones, so that a binary from before the rename kept purging
-- through a rolling update or a rollback. No tagged release ever shipped the old names (only the dev pre-releases
-- v0.0.0-dev.1 to .3 ran them), so no such binary is left to keep, and this migration drops them: the two overloads of
-- courier_audit_purge that 0006 and 0020 created, and the delete trigger's reading of the courier.audit_retention
-- setting. The trigger lets a DELETE through only while agentkourier.audit_retention is on, which
-- agentkourier_audit_purge sets for its own transaction.
CREATE OR REPLACE FUNCTION audit_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('agentkourier.audit_retention', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit is append-only' USING ERRCODE = 'restrict_violation';
END
$$;

DROP FUNCTION IF EXISTS courier_audit_purge(timestamptz);
DROP FUNCTION IF EXISTS courier_audit_purge(timestamptz, integer);

-- The user a turn is filed under when nobody wrote it (session.SystemUser) is stored in the reply queue and in the
-- session's current turn; it is now agent-kourier:system, and a turn queued or running under the old value would
-- read as a person's. The audit table holds no such value: its rows are append-only, and an audit row of a system
-- turn names no user. The decider an auto-denied approval was recorded under (interact's autoDecider) was renamed
-- from courier to agent-kourier when the project was; the rows written before that carry the old name.
UPDATE reply_queue SET user_id = 'agent-kourier:system' WHERE user_id = 'courier:system';
UPDATE sessions SET turn_user = 'agent-kourier:system' WHERE turn_user = 'courier:system';
UPDATE interactions SET decider = 'agent-kourier' WHERE decider = 'courier';
