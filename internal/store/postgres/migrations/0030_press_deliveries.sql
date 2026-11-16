-- The delivery key of each button press recorded, so that a press the chat platform sends again (a retry of a request whose
-- answer was lost, or an envelope sent again) is recorded once, and its person told once. A key outlives its press, which is
-- deleted once it is processed, since the second delivery may come after that: PurgePresses forgets it once the grace has
-- passed since the press. seq is the press's (interaction_presses.seq, never given again): a delivery that finds its press still
-- waiting hands that press to the worker, since the first delivery's caller may have seen an error from a commit that succeeded,
-- and not told the worker. The key is the platform adapter's, and opaque here; a press with no key has no row. Additive: an older
-- binary does not read the table, and writes no keys, so a press it records is recorded each time it comes, as before.
CREATE TABLE press_deliveries (
    connection   text NOT NULL,
    delivery_key text NOT NULL,
    seq          bigint NOT NULL,
    pressed_at   timestamptz NOT NULL,
    PRIMARY KEY (connection, delivery_key)
);
CREATE INDEX press_deliveries_pressed ON press_deliveries (pressed_at);
