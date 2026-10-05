-- When courier gave up on posting the row; NULL if it did not. (SQLite stores 0.)
ALTER TABLE outbox ADD COLUMN abandoned_at timestamptz;
