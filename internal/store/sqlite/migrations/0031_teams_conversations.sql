-- What Microsoft Teams tells the bot of a channel only in the activities it sends it, one row for each channel of a teams
-- connection: the Bot Connector to post to the channel through (service_url), the team's Teams ID and Microsoft Entra group ID
-- (which Graph names the team by), and the tenant. The receiver merges each verified activity into the row, and a field the
-- activity leaves out keeps its value. updated_at is when the row was last written. Kept across a restart and a failover, since an
-- alert read from Graph may come before any activity. No retention: one small row for each channel the bot has heard from.
-- Additive: an older binary does not read the table.
CREATE TABLE teams_conversations (
    connection   TEXT NOT NULL,
    channel      TEXT NOT NULL,
    service_url  TEXT NOT NULL,
    team_id      TEXT NOT NULL,
    aad_group_id TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (connection, channel)
);
