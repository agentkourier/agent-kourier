-- Session origins are generic source and subject metadata. Existing alert names were recorded
-- before the source was persisted, so their source is preserved as legacy at this boundary.
-- This migration drops the old alert-specific column: an older binary cannot read this session
-- schema after the upgrade and must not be rolled back against it.
ALTER TABLE sessions ADD COLUMN origin_source TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN origin_subject TEXT NOT NULL DEFAULT '';
UPDATE sessions SET origin_source = 'legacy', origin_subject = alert_name WHERE alert_name <> '';
ALTER TABLE sessions DROP COLUMN alert_name;
