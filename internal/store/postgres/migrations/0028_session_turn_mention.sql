-- The bot's mention whose turn began a session's task, if one did. An answer moves the turn's message, and this stays, so a mention
-- is known for as long as its task runs. Additive: an older binary ignores it.
ALTER TABLE sessions ADD COLUMN turn_mention text NOT NULL DEFAULT '';
