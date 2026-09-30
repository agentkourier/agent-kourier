CREATE TABLE audit (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    at       INTEGER NOT NULL,
    kind     TEXT NOT NULL
                 CHECK (kind IN ('event', 'agent_call', 'interaction_decision', 'config_change')),
    user_id  TEXT NOT NULL DEFAULT '',
    email    TEXT NOT NULL DEFAULT '',
    identity TEXT NOT NULL DEFAULT '',
    binding  TEXT NOT NULL DEFAULT '',
    subject  TEXT NOT NULL,
    outcome  TEXT NOT NULL
);
CREATE INDEX audit_at ON audit (at, sequence);

-- Append-only: rows can be neither changed nor removed.
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;
