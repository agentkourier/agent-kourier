ALTER TABLE interactions ADD COLUMN text_replies INTEGER NOT NULL DEFAULT 0 CHECK (text_replies IN (0, 1));
