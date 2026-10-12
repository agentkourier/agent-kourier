ALTER TABLE reply_queue ADD COLUMN acknowledgment text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_message_ts text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_acknowledgment text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN turn_acknowledgment_id text NOT NULL DEFAULT '';
