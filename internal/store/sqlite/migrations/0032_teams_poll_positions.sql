-- Where the leader's poll of each channel of a teams connection with no notificationURL stands, one row for each polled channel: the
-- @odata.deltaLink of the channel's last delta read of Microsoft Graph whose messages were all handed on (delta_link, which carries
-- Graph's state token), and when the poll that made that read began (polled_at). A run of the connection resumes the channel's delta
-- from it, so a restart or a change of leader reads what was posted meanwhile. The poller deletes a channel's row when no Binding names
-- the channel any longer, and the leader a connection's rows when its ChatConnection is deleted. No retention: one small row for each
-- polled channel, which a later poll of the channel replaces. Additive: an older binary does not read the table.
CREATE TABLE teams_poll_positions (
    connection TEXT NOT NULL,
    channel    TEXT NOT NULL,
    delta_link TEXT NOT NULL,
    polled_at  INTEGER NOT NULL,
    PRIMARY KEY (connection, channel)
);
