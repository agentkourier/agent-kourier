-- Audit retention (data protection, DP-1): the purge in batches. courier_audit_purge(cutoff) of 0006 deletes every
-- entry before the cutoff in one statement, which a log of years can make longer than the statement timeout; this
-- overload deletes at most batch of the oldest, and the store calls it until it returns fewer than it was given.
-- Additive: a new function beside the old one, which stays. The setting that lets the delete trigger through is set
-- only inside the function, local to the transaction, and switched off again before it returns, as in 0006.
CREATE FUNCTION courier_audit_purge(cutoff timestamptz, batch integer) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    purged bigint;
BEGIN
    PERFORM set_config('courier.audit_retention', 'on', true);
    DELETE FROM audit WHERE sequence IN (SELECT sequence FROM audit WHERE at < cutoff ORDER BY at, sequence LIMIT batch);
    GET DIAGNOSTICS purged = ROW_COUNT;
    PERFORM set_config('courier.audit_retention', 'off', true);
    RETURN purged;
END
$$;
