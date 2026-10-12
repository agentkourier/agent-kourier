ALTER TABLE reply_queue ADD COLUMN acknowledgment TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_message_ts TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_acknowledgment TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_acknowledgment_id TEXT NOT NULL DEFAULT '';
