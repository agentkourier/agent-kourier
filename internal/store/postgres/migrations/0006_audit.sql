-- The audit log is append-only against the application. The sequence is an identity column, and the
-- store serialises appends with an advisory lock so that an entry is shown only after every entry with
-- a lower sequence is (see package postgres).
CREATE TABLE audit (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at       timestamptz NOT NULL,
    kind     text NOT NULL
                 CHECK (kind IN ('event', 'agent_call', 'interaction_decision', 'config_change')),
    user_id  text NOT NULL DEFAULT '',
    email    text NOT NULL DEFAULT '',
    identity text NOT NULL DEFAULT '',
    binding  text NOT NULL DEFAULT '',
    subject  text NOT NULL,
    outcome  text NOT NULL
);
CREATE INDEX audit_at ON audit (at, sequence);

-- Rows can be neither changed nor removed. The one way out is the retention purge: courier_audit_purge
-- deletes the entries older than a cutoff, and only inside it does the transaction carry the setting
-- that lets a DELETE through. The trigger is written for retention from the start, so the retention
-- work needs no migration that drops and recreates it.
--
-- This is a guard against a bug in the application, as SQLite's triggers are: whoever owns the table
-- can disable the trigger or set the setting, and nothing here is tamper-evident.
CREATE FUNCTION audit_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('courier.audit_retention', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit is append-only' USING ERRCODE = 'restrict_violation';
END
$$;

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
    FOR EACH ROW EXECUTE FUNCTION audit_guard();
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
    FOR EACH ROW EXECUTE FUNCTION audit_guard();
CREATE TRIGGER audit_no_truncate BEFORE TRUNCATE ON audit
    FOR EACH STATEMENT EXECUTE FUNCTION audit_guard();

-- courier_audit_purge deletes the entries from before the cutoff and returns how many. The setting is
-- local to the transaction and switched off again before the function returns.
CREATE FUNCTION courier_audit_purge(cutoff timestamptz) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    purged bigint;
BEGIN
    PERFORM set_config('courier.audit_retention', 'on', true);
    DELETE FROM audit WHERE at < cutoff;
    GET DIAGNOSTICS purged = ROW_COUNT;
    PERFORM set_config('courier.audit_retention', 'off', true);
    RETURN purged;
END
$$;
