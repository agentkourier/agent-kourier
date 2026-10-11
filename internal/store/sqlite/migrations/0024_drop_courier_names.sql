-- The user a turn is filed under when nobody wrote it (session.SystemUser) is now agent-kourier:system, and a turn
-- queued or running under the old value would read as a person's. It is stored in the reply queue and in the session's
-- current turn; the audit table holds no such value (its rows are append-only, and an audit row of a system turn
-- names no user). The decider an auto-denied approval was recorded under (interact's autoDecider) was renamed from
-- courier to agent-kourier when the project was; the rows written before that carry the old name. The audit purge
-- functions and setting that carried the old name exist only in Postgres, whose 0024 drops them; this store has
-- nothing else to change. No tagged release ever shipped the old values (only the dev pre-releases ran them).
UPDATE reply_queue SET user_id = 'agent-kourier:system' WHERE user_id = 'courier:system';
UPDATE sessions SET turn_user = 'agent-kourier:system' WHERE turn_user = 'courier:system';
UPDATE interactions SET decider = 'agent-kourier' WHERE decider = 'courier';
