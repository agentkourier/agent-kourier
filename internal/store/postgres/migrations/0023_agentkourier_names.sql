-- The project is now Agent Kourier. Migrations 0006 and 0020 named the audit purge functions, and the setting that
-- lets the delete trigger through, after the old project name, and an applied migration is not edited.
--
-- This one is additive, as a migration is for one release (hardening-before-pilot.md): it creates the purge functions
-- under the new names, and lets the delete trigger accept either retention setting. courier_audit_purge of 0006 and
-- 0020 stays, so the binary from before the rename, which a rolling update runs beside the new one and a rollback
-- brings back, keeps purging. A later migration drops the old functions once no such binary can run.
CREATE OR REPLACE FUNCTION audit_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND (current_setting('agentkourier.audit_retention', true) = 'on'
                             OR current_setting('courier.audit_retention', true) = 'on') THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit is append-only' USING ERRCODE = 'restrict_violation';
END
$$;

CREATE FUNCTION agentkourier_audit_purge(cutoff timestamptz) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    purged bigint;
BEGIN
    PERFORM set_config('agentkourier.audit_retention', 'on', true);
    DELETE FROM audit WHERE at < cutoff;
    GET DIAGNOSTICS purged = ROW_COUNT;
    PERFORM set_config('agentkourier.audit_retention', 'off', true);
    RETURN purged;
END
$$;

CREATE FUNCTION agentkourier_audit_purge(cutoff timestamptz, batch integer) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    purged bigint;
BEGIN
    PERFORM set_config('agentkourier.audit_retention', 'on', true);
    DELETE FROM audit WHERE sequence IN (SELECT sequence FROM audit WHERE at < cutoff ORDER BY at, sequence LIMIT batch);
    GET DIAGNOSTICS purged = ROW_COUNT;
    PERFORM set_config('agentkourier.audit_retention', 'off', true);
    RETURN purged;
END
$$;
