-- A button press, recorded before it is processed and deleted once it is. It holds what the person
-- typed before anything is decided, so it does not outlive its interaction by long (PurgePresses).
-- interaction_id has no foreign key: it is the button's value, and a press may carry one we never made.
CREATE TABLE interaction_presses (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    interaction_id TEXT NOT NULL,
    user_id        TEXT NOT NULL,
    channel        TEXT NOT NULL,
    message_ts     TEXT NOT NULL,
    inputs         BLOB NOT NULL,
    pressed_at     INTEGER NOT NULL
);
CREATE INDEX interaction_presses_user ON interaction_presses (user_id);
CREATE INDEX interaction_presses_interaction ON interaction_presses (interaction_id, pressed_at);
