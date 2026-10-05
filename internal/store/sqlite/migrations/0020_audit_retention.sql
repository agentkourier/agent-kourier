-- Audit retention (data protection, DP-1): the one way an entry leaves the log is the retention purge.
--
-- The delete trigger of 0006 refused every DELETE. It now lets one through while a row of audit_purge exists whose
-- cutoff is later than the entry's time, and only then: the purge (Store.PurgeAudit) inserts that row, deletes, and
-- removes the row again in one transaction, which SQLite's single write connection makes the only writer that can
-- see it, and which a rollback or a crash leaves with no row at all. A row left there by hand would still allow
-- only the entries before its cutoff. The UPDATE trigger is not touched.
--
-- This is Postgres's setting (courier.audit_retention, migration 0006) as far as SQLite allows it, and with the
-- same threat model: the log is append-only against the application's own code paths, so that a bug cannot edit
-- or erase it. Whoever can write the file can drop the trigger or insert the row, and nothing here is tamper-evident.
CREATE TABLE audit_purge (cutoff INTEGER NOT NULL);

DROP TRIGGER audit_no_delete;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
WHEN NOT EXISTS (SELECT 1 FROM audit_purge WHERE OLD.at < cutoff)
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;
