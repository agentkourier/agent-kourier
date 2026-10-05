-- A button press, recorded before it is processed and deleted once it is. It holds what the person
-- typed before anything is decided, so it does not outlive its interaction by long (PurgePresses).
-- interaction_id has no foreign key: it is the button's value, and a press may carry one we never made.
CREATE TABLE interaction_presses (
    seq            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    interaction_id text NOT NULL,
    user_id        text NOT NULL,
    channel        text NOT NULL,
    message_ts     text NOT NULL,
    inputs         bytea NOT NULL,
    pressed_at     timestamptz NOT NULL
);
CREATE INDEX interaction_presses_user ON interaction_presses (user_id);
CREATE INDEX interaction_presses_interaction ON interaction_presses (interaction_id, pressed_at);
