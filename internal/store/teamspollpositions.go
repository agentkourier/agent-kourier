package store

import (
	"context"
	"time"
)

// TeamsPollPosition is where the leader's poll of a Microsoft Teams channel's messages stands: the deltaLink of the channel's last delta
// read whose messages have all been handed on, and when the poll that made that read began. A run of the connection resumes the channel's
// delta from it, so that a restart or a change of leader reads what was posted meanwhile.
type TeamsPollPosition struct {
	// Connection is the ChatConnection ("namespace/name") and Channel the channel's ID (19:…@thread.tacv2): together, the key.
	Connection, Channel string
	// DeltaLink is the @odata.deltaLink that Microsoft Graph gave the read. It carries Graph's state token: it is never logged.
	DeltaLink string
	// PolledAt is when the poll that made the read began.
	PolledAt time.Time
}

// TeamsPollPositions keeps one TeamsPollPosition for each polled channel of each Microsoft Teams connection. Only the leader polls, and a
// write of a fenced store that is not the leader is ErrDeposed, as every write is. No retention loop deletes a position: the poller
// deletes a channel's when no Binding names the channel any longer, and at a run's start those of the channels that no Binding names
// (DeleteTeamsPollPositionsExcept), the broker deletes a connection's when it finds the connection's
// ChatConnection deleted (DeleteTeamsPollPositions), and a position left over is one small row that a later poll of its channel replaces.
// A position holds IDs, a URL and a time, no message content.
type TeamsPollPositions interface {
	// SaveTeamsPollPosition replaces the position of p.Connection and p.Channel with p, and makes it if there is none. ErrInvalid if
	// Connection, Channel or DeltaLink is empty, or PolledAt is not after the Unix epoch.
	SaveTeamsPollPosition(ctx context.Context, p TeamsPollPosition) error
	// GetTeamsPollPosition returns the position of channel of connection, or ErrNotFound. ErrInvalid if either is empty.
	GetTeamsPollPosition(ctx context.Context, connection, channel string) (TeamsPollPosition, error)
	// DeleteTeamsPollPosition deletes the position of channel of connection, compared byte for byte; a channel with none is not an
	// error. ErrInvalid if either is empty.
	DeleteTeamsPollPosition(ctx context.Context, connection, channel string) error
	// DeleteTeamsPollPositions deletes every position of connection, compared byte for byte, and none of another; a connection with
	// none is not an error. ErrInvalid if connection is empty.
	DeleteTeamsPollPositions(ctx context.Context, connection string) error
	// DeleteTeamsPollPositionsExcept deletes every position of connection whose channel is not in keep, compared byte for byte, in one
	// statement, and none of another connection; an empty keep deletes them all. ErrInvalid if connection, or a channel of keep, is
	// empty.
	DeleteTeamsPollPositionsExcept(ctx context.Context, connection string, keep []string) error
}
