package store

import (
	"context"
	"time"
)

// TeamsConversation is what Microsoft Teams tells the bot of a channel only in the activities it sends the bot: the Bot Connector to
// post to the channel through, and the team the channel is in. The receiver of a teams connection records it from every verified
// activity, the bot's install events included. Posting reads the service URL and Graph the team's group ID, and an alert read from
// Graph may come before any activity, so the record is kept across a restart and a leader's failover.
type TeamsConversation struct {
	// Connection is the ChatConnection ("namespace/name") and Channel the channel's ID (19:…@thread.tacv2): together, the key.
	Connection, Channel string
	// ServiceURL is the base URL of the Bot Connector for the channel, the activity's serviceUrl.
	ServiceURL string
	// TeamID is the team's Teams ID, and AADGroupID the team's Microsoft Entra group ID, which Graph names the team by.
	TeamID, AADGroupID string
	// TenantID is the ID of the tenant the team is in.
	TenantID string
	// UpdatedAt is when the record was last remembered.
	UpdatedAt time.Time
}

// TeamsConversations keeps one TeamsConversation for each channel of each Microsoft Teams connection. Only the leader receives a
// connection's activities, and a write of a fenced store that is not the leader is ErrDeposed, as every write is. No retention loop
// deletes a record: there is one for each channel the bot has heard from, and a channel's last service URL does no harm once it is
// quiet.
type TeamsConversations interface {
	// RememberTeamsConversation merges c into the record of c.Connection and c.Channel, and makes the record if there is none: a field
	// that c leaves empty keeps the value it had (an activity that leaves a field out says nothing of it), one that c sets replaces it,
	// and UpdatedAt becomes c.UpdatedAt. Calls that run together for one channel each keep what they set. ErrInvalid if Connection or
	// Channel is empty, or UpdatedAt is not after the Unix epoch.
	RememberTeamsConversation(ctx context.Context, c TeamsConversation) error
	// GetTeamsConversation returns the record of channel of connection, or ErrNotFound. ErrInvalid if either is empty.
	GetTeamsConversation(ctx context.Context, connection, channel string) (TeamsConversation, error)
}
