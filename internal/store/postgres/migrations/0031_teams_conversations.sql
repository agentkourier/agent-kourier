-- What Microsoft Teams tells the bot of a channel only in the activities it sends it, one row for each channel of a teams
-- connection: the Bot Connector to post to the channel through (service_url), the team's Teams ID and Microsoft Entra group ID
-- (which Graph names the team by), and the tenant. The receiver merges each verified activity into the row, and a field the
-- activity leaves out keeps its value. updated_at is when the row was last written. Kept across a restart and a failover, since an
-- alert read from Graph may come before any activity. No retention: one small row for each channel the bot has heard from.
-- Additive: an older binary does not read the table.
CREATE TABLE teams_conversations (
    connection   text NOT NULL,
    channel      text NOT NULL,
    service_url  text NOT NULL,
    team_id      text NOT NULL,
    aad_group_id text NOT NULL,
    tenant_id    text NOT NULL,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (connection, channel)
);
