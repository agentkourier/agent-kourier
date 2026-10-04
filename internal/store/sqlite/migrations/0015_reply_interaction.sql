ALTER TABLE reply_queue ADD COLUMN interaction_id TEXT NOT NULL DEFAULT '';
CREATE INDEX reply_queue_pending_interaction ON reply_queue (interaction_id, enqueued_at) WHERE delivered_at IS NULL;
